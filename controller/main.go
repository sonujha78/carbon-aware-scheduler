package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	labelDeferrable = "green.devops/deferrable"
	annDeadline     = "green.devops/deadline"
	annRuntime      = "green.devops/estimated-runtime"
	annPower        = "green.devops/power-watts"
	annBaseline     = "green.devops/baseline-intensity"
	annAvoided      = "green.devops/co2-avoided-grams"
)

var (
	currentIntensity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "carbon_current_intensity", Help: "Current grid carbon intensity in gCO2/kWh."})
	jobsWaiting = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "carbon_jobs_waiting", Help: "Deferrable jobs currently suspended."})
	jobsSuspended = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "carbon_jobs_suspended_total", Help: "Jobs suspended because the grid was dirty."})
	jobsResumed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "carbon_jobs_resumed_total", Help: "Jobs resumed."})
	forcedRuns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "carbon_deadline_forced_runs_total", Help: "Jobs forced to run because of their deadline."})
	co2Avoided = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "carbon_estimated_co2_avoided_grams_total", Help: "Estimated grams of CO2 avoided by delaying jobs."})
)

type Controller struct {
	client    kubernetes.Interface
	http      *http.Client
	carbonURL string
	threshold float64
}

func ptr[T any](v T) *T { return &v }

func isFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func annFloat(job *batchv1.Job, key string, def float64) float64 {
	if v, ok := job.Annotations[key]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func annDuration(job *batchv1.Job, key string, def time.Duration) time.Duration {
	if v, ok := job.Annotations[key]; ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func (c *Controller) fetchIntensity(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.carbonURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("carbon API returned %s", resp.Status)
	}
	var body struct {
		Intensity float64 `json:"intensity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.Intensity, nil
}

func (c *Controller) reconcile(ctx context.Context) error {
	intensity, err := c.fetchIntensity(ctx)
	if err != nil {
		return fmt.Errorf("fetch intensity: %w", err)
	}
	currentIntensity.Set(intensity)
	dirty := intensity > c.threshold

	jobs, err := c.client.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: labelDeferrable + "=true",
	})
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	waiting := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if isFinished(job) {
			continue
		}
		if err := c.reconcileJob(ctx, job, intensity, dirty); err != nil {
			log.Printf("job %s/%s: %v", job.Namespace, job.Name, err)
			continue
		}
		if job.Spec.Suspend != nil && *job.Spec.Suspend {
			waiting++
		}
	}
	jobsWaiting.Set(float64(waiting))
	log.Printf("intensity=%.1f threshold=%.0f dirty=%v deferrable_jobs=%d waiting=%d",
		intensity, c.threshold, dirty, len(jobs.Items), waiting)
	return nil
}

func (c *Controller) reconcileJob(ctx context.Context, job *batchv1.Job, intensity float64, dirty bool) error {
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	changed := false

	// Remember the intensity when we first saw the job: that is what "run now" would have cost.
	if _, ok := job.Annotations[annBaseline]; !ok {
		job.Annotations[annBaseline] = strconv.FormatFloat(intensity, 'f', 1, 64)
		changed = true
	}

	est := annDuration(job, annRuntime, time.Minute)
	mustRun := false
	if raw, ok := job.Annotations[annDeadline]; ok {
		deadline, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			log.Printf("job %s/%s: bad deadline %q (want RFC3339)", job.Namespace, job.Name, raw)
		} else {
			mustRun = time.Now().Add(est).After(deadline)
		}
	}

	suspended := job.Spec.Suspend != nil && *job.Spec.Suspend
	shouldRun := !dirty || mustRun
	name := job.Namespace + "/" + job.Name

	var onSuccess func()
	switch {
	case shouldRun && suspended:
		job.Spec.Suspend = ptr(false)
		changed = true
		if dirty && mustRun {
			log.Printf("RESUME (deadline forced) %s at %.1f gCO2/kWh", name, intensity)
			onSuccess = func() { forcedRuns.Inc(); jobsResumed.Inc() }
		} else {
			baseline := annFloat(job, annBaseline, intensity)
			watts := annFloat(job, annPower, 100)
			avoided := math.Max(0, baseline-intensity) * watts / 1000 * est.Hours()
			job.Annotations[annAvoided] = strconv.FormatFloat(avoided, 'f', 3, 64)
			log.Printf("RESUME %s at %.1f gCO2/kWh (baseline %.1f, ~%.3f g CO2 avoided)", name, intensity, baseline, avoided)
			onSuccess = func() { jobsResumed.Inc(); co2Avoided.Add(avoided) }
		}
	case !shouldRun && !suspended:
		job.Spec.Suspend = ptr(true)
		changed = true
		log.Printf("SUSPEND %s at %.1f gCO2/kWh", name, intensity)
		onSuccess = func() { jobsSuspended.Inc() }
	}

	if !changed {
		return nil
	}
	if _, err := c.client.BatchV1().Jobs(job.Namespace).Update(ctx, job, metav1.UpdateOptions{}); err != nil {
		return err
	}
	if onSuccess != nil {
		onSuccess()
	}
	return nil
}

func main() {
	carbonURL := flag.String("carbon-url", "http://localhost:8081/intensity", "carbon intensity API endpoint")
	threshold := flag.Float64("threshold", 250, "gCO2/kWh above which deferrable jobs are suspended")
	interval := flag.Duration("interval", 10*time.Second, "reconcile interval")
	metricsAddr := flag.String("metrics-addr", ":9095", "metrics listen address")
	kubeconfig := flag.String("kubeconfig", filepath.Join(os.Getenv("HOME"), ".kube", "config"), "kubeconfig path (used when not in-cluster)")
	flag.Parse()

	prometheus.MustRegister(currentIntensity, jobsWaiting, jobsSuspended, jobsResumed, forcedRuns, co2Avoided)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			log.Fatalf("kube config: %v", err)
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
		log.Printf("metrics on %s/metrics", *metricsAddr)
		if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
			log.Fatalf("metrics server: %v", err)
		}
	}()

	c := &Controller{
		client:    client,
		http:      &http.Client{Timeout: 5 * time.Second},
		carbonURL: *carbonURL,
		threshold: *threshold,
	}
	log.Printf("carbon controller started: threshold=%.0f interval=%s", *threshold, *interval)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if err := c.reconcile(ctx); err != nil {
			log.Printf("reconcile error: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
		}
	}
}

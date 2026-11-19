package main

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newJob(suspended bool, created time.Time, ann map[string]string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "j",
			Namespace:         "default",
			Labels:            map[string]string{labelDeferrable: "true"},
			Annotations:       ann,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: batchv1.JobSpec{Suspend: ptr(suspended)},
	}
}

// reconcile runs one pass on a job and returns the stored result.
func reconcile(t *testing.T, job *batchv1.Job, intensity float64, p Policy) *batchv1.Job {
	t.Helper()
	ctx := context.Background()
	cs := fake.NewSimpleClientset(job.DeepCopy())
	c := &Controller{client: cs}
	if err := c.reconcileJob(ctx, job.DeepCopy(), intensity, intensity > p.Threshold, p); err != nil {
		t.Fatalf("reconcileJob: %v", err)
	}
	got, err := cs.BatchV1().Jobs("default").Get(ctx, "j", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return got
}

func isSuspended(j *batchv1.Job) bool { return j.Spec.Suspend != nil && *j.Spec.Suspend }

func TestDirtyGridSuspendsRunningJob(t *testing.T) {
	got := reconcile(t, newJob(false, time.Now(), nil), 400, Policy{Threshold: 250})
	if !isSuspended(got) {
		t.Fatal("running job should be suspended when the grid is dirty")
	}
}

func TestCleanGridLeavesRunningJobAlone(t *testing.T) {
	got := reconcile(t, newJob(false, time.Now(), nil), 150, Policy{Threshold: 250})
	if isSuspended(got) {
		t.Fatal("running job must not be suspended on a clean grid")
	}
}

func TestCleanGridResumesAndRecordsAvoidedCO2(t *testing.T) {
	ann := map[string]string{
		annBaseline: "400",
		annPower:    "150",
		annRuntime:  "1m",
	}
	got := reconcile(t, newJob(true, time.Now(), ann), 150, Policy{Threshold: 250})
	if isSuspended(got) {
		t.Fatal("suspended job should resume when the grid is clean")
	}
	if v := got.Annotations[annAvoided]; v != "0.625" {
		t.Fatalf("avoided CO2 = %q, want 0.625", v)
	}
}

func TestDeadlineForcesRunOnDirtyGrid(t *testing.T) {
	ann := map[string]string{
		annDeadline: time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339),
		annRuntime:  "1m",
	}
	got := reconcile(t, newJob(true, time.Now(), ann), 400, Policy{Threshold: 250})
	if isSuspended(got) {
		t.Fatal("job close to its deadline must run even on a dirty grid")
	}
}

func TestFarDeadlineKeepsJobWaiting(t *testing.T) {
	ann := map[string]string{
		annDeadline: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		annRuntime:  "1m",
	}
	got := reconcile(t, newJob(true, time.Now(), ann), 400, Policy{Threshold: 250})
	if !isSuspended(got) {
		t.Fatal("job with a distant deadline should keep waiting on a dirty grid")
	}
}

func TestMaxDelayForcesRun(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour)
	got := reconcile(t, newJob(true, old, nil), 400, Policy{Threshold: 250, MaxDelay: 2 * time.Hour})
	if isSuspended(got) {
		t.Fatal("job waiting longer than maxDelay must be forced to run")
	}
}

package main

import (
	"context"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var policyGVR = schema.GroupVersionResource{
	Group: "green.devops", Version: "v1alpha1", Resource: "carbonpolicies",
}

// Policy is the effective configuration for one reconcile loop.
type Policy struct {
	Threshold  float64
	Namespaces []string
	MaxDelay   time.Duration
	FromCRD    bool
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

// loadPolicy reads the CarbonPolicy; if it does not exist, flag defaults are used.
func (c *Controller) loadPolicy(ctx context.Context) Policy {
	p := c.defaults
	obj, err := c.dyn.Resource(policyGVR).Get(ctx, c.policyName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("policy %q: %v (using flag defaults)", c.policyName, err)
		}
		return p
	}
	p.FromCRD = true
	if v, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "thresholdGramsPerKwh"); found {
		if f, ok := toFloat(v); ok {
			p.Threshold = f
		}
	}
	if v, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "maxDelayHours"); found {
		if f, ok := toFloat(v); ok && f > 0 {
			p.MaxDelay = time.Duration(f * float64(time.Hour))
		}
	}
	if ns, found, _ := unstructured.NestedStringSlice(obj.Object, "spec", "namespaces"); found {
		p.Namespaces = ns
	}
	return p
}

// updateStatus writes the live state back to the CarbonPolicy status.
func (c *Controller) updateStatus(ctx context.Context, intensity float64, dirty bool, waiting int) {
	obj, err := c.dyn.Resource(policyGVR).Get(ctx, c.policyName, metav1.GetOptions{})
	if err != nil {
		return
	}
	obj.Object["status"] = map[string]interface{}{
		"currentIntensity":  intensity,
		"dirty":             dirty,
		"waitingJobs":       int64(waiting),
		"lastReconcileTime": time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := c.dyn.Resource(policyGVR).UpdateStatus(ctx, obj, metav1.UpdateOptions{}); err != nil {
		log.Printf("policy status update: %v", err)
	}
}

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	kube "github.com/kedify/kedr/internal/kubernetes"
	"github.com/kedify/kedr/internal/model"
	prom "github.com/kedify/kedr/internal/prometheus"
	"github.com/kedify/kedr/internal/recommend"
	"github.com/kedify/kedr/internal/report"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiscoveryMetricsAnalyzerReportIntegration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	yes := true
	container := corev1.Container{Name: "main", Image: "app:1", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}}
	selector := map[string]string{"app": "app"}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns", UID: "workload", Generation: 1, Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"}}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector}, Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1}}
	set := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", UID: "rs-uid", Labels: selector, Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "app", UID: "workload", Controller: &yes}}}, Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pod-template-hash": "release"}}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "pod-uid", Labels: selector, CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour)), OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", UID: "rs-uid", Controller: &yes}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{container}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	cfg := config.Default("simple")
	cfg.NamespaceValues, cfg.ResourceValues = []string{"ns"}, []string{"Deployment"}
	cfg.HistoryDuration, cfg.MinimumHistoryHours, cfg.TimeframeDuration = 24, 3, 1
	cfg.UseOOMKillData, cfg.DetectMemoryLeaks = true, true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	loader := kube.NewLoader(cfg)
	clients := kube.Clients{Typed: fake.NewSimpleClientset(deployment, set, pod)}
	objects, err := loader.List(context.Background(), clients)
	if err != nil || len(objects) != 1 {
		t.Fatalf("discovery: %+v %v", objects, err)
	}
	object, err := loader.Observe(context.Background(), clients, objects[0])
	if err != nil {
		t.Fatal(err)
	}
	client, err := prom.New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.UseHTTPClient(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "rate(") || strings.Contains(query, "quantile") || strings.Contains(query, "max_over_time") {
			t.Errorf("pre-aggregated query: %s", query)
		}
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		step, _ := strconv.ParseInt(r.URL.Query().Get("step"), 10, 64)
		if r.URL.Path == "/api/v1/query" {
			at, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
			match := regexp.MustCompile(`\[(\d+)ms\]$`).FindStringSubmatch(query)
			if len(match) != 2 {
				t.Fatalf("native range selector required: %s", query)
			}
			duration, _ := strconv.ParseInt(match[1], 10, 64)
			end = int64(at)
			start, step = end-duration/1000+60, 60
		}
		if step <= 0 {
			t.Fatal("invalid query step")
		}
		points := make([][]any, 0)
		for timestamp := start; timestamp <= end; timestamp += step {
			source := timestamp
			age := float64(source - now.Add(-24*time.Hour).Unix())
			value := (100 + max(0, age)/3600*12) * 1024 * 1024
			switch {
			case strings.Contains(query, "last_terminated_timestamp"):
				value = float64(now.Add(-30 * time.Minute).Unix())
			case strings.Contains(query, "cpu_usage_seconds"):
				value = float64(source-now.Add(-48*time.Hour).Unix()) * .2
			}
			points = append(points, []any{timestamp, strconv.FormatFloat(value, 'f', -1, 64)})
		}
		body, marshalErr := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"namespace": "ns", "pod": "pod", "container": "main", "uid": "pod-uid", "id": "container-lifetime", "job": "kubelet"}, "values": points}}}})
		if marshalErr != nil {
			return nil, marshalErr
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})})
	metrics, warnings := client.Gather(context.Background(), object)
	if len(warnings) > 0 {
		t.Fatalf("collection warnings: %v", warnings)
	}
	object.Warnings = append(object.Warnings, warnings...)
	result, err := strategy.Run(cfg, metrics, object)
	if err != nil {
		t.Fatal(err)
	}
	memory := result.Analysis.Results[0]
	if memory.MemoryLeak == nil || memory.MemoryLeak.Status != analysis.MemoryLeakPotential || len(memory.Evidence.OOMKills) != 1 || memory.OOMAdjustment == nil {
		t.Fatalf("diagnostics lost across adapters: %+v", memory)
	}
	scan := recommend.Scan(object, result)
	if scan.Recommended.Requests[model.CPU].Value.Unknown || scan.Recommended.Requests[model.CPU].Value.Value < .199 || scan.Recommended.Requests[model.CPU].Value.Value > .201 {
		t.Fatalf("CPU units/rate changed: %+v", scan)
	}
	for _, format := range []string{"json", "yaml", "table", "csv", "html"} {
		cfg.Format = format
		text, renderErr := report.Render(model.Report{Scans: []model.Scan{scan}}, cfg, false)
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		for _, notice := range []string{"oom-kill-detected", "potential-memory-leak"} {
			if !strings.Contains(text, notice) {
				t.Fatalf("%s report lost %s", format, notice)
			}
		}
	}
}

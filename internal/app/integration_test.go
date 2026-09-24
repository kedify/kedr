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
	"github.com/kedify/kedr/internal/explain"
	kube "github.com/kedify/kedr/internal/kubernetes"
	"github.com/kedify/kedr/internal/model"
	prom "github.com/kedify/kedr/internal/prometheus"
	"github.com/kedify/kedr/internal/recommend"
	"github.com/kedify/kedr/internal/report"
	"github.com/kedify/kedr/internal/runstore"
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
	for _, name := range []string{"simple", "simple_limit"} {
		for _, kind := range []string{"Deployment", model.StandalonePodKind} {
			t.Run(name+"/"+kind, func(t *testing.T) { discoveryMetricsAnalyzerReport(t, name, kind) })
		}
	}
}

func discoveryMetricsAnalyzerReport(t *testing.T, strategyName, kind string) {
	t.Helper()
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
	if kind == model.StandalonePodKind {
		pod.OwnerReferences = nil
	}
	cfg := config.Default(strategyName)
	cfg.NamespaceValues, cfg.ResourceValues = []string{"ns"}, []string{kind}
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
	if kind == model.StandalonePodKind {
		// The API pod identity is sufficient; standalone pods need no KSM owner
		// queries or historical membership reconstruction before usage collection.
		object = client.HistoricalPods(context.Background(), object)
	}
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
	store := runstore.Store{Root: t.TempDir()}
	saved, err := store.Save(runstore.Run{Strategy: cfg.Strategy, Rows: []runstore.Row{runstore.NewRow(scan, metrics, runstore.Connection{})}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	explanation := explain.Build(loaded, loaded.Rows[0])
	reloadedMetrics, reloadedWarnings := client.GatherWindow(context.Background(), loaded.Rows[0].Object(), time.UnixMilli(metrics.WindowStart), time.UnixMilli(metrics.EvaluationTime))
	explanation.AddMetrics(loaded.Rows[0], reloadedMetrics, reloadedWarnings)
	if !strings.Contains(explanation.ChartStatus, "Verified") || len(explanation.Cards) != 4 {
		t.Fatalf("saved explanation failed: %+v", explanation)
	}
	if _, err := explain.HTML(explanation); err != nil {
		t.Fatal(err)
	}

	if scan.Recommended.Requests[model.CPU].Value.Unknown || scan.Recommended.Requests[model.CPU].Value.Value < .199 || scan.Recommended.Requests[model.CPU].Value.Value > .201 {
		t.Fatalf("CPU units/rate changed: %+v", scan)
	}
	for _, format := range []string{"json", "yaml", "table", "csv", "html"} {
		cfg.Format = format
		cfg.Explain = true
		text, renderErr := report.Render(model.Report{Scans: []model.Scan{scan}}, cfg, false)
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		if !strings.Contains(text, kind) {
			t.Fatalf("%s report lost workload kind %s", format, kind)
		}
		for _, notice := range []string{"oom-kill-detected", "potential-memory-leak"} {
			if !strings.Contains(text, notice) {
				t.Fatalf("%s report lost %s", format, notice)
			}
		}
	}
}

func TestSavedRowKeyIncludesAllIdentityComponents(t *testing.T) {
	a, b := "cluster-a", "cluster-b"
	objects := []model.Object{
		{Name: "same", Namespace: "a", Kind: "Deployment", Container: "main", Cluster: &a},
		{Name: "same", Namespace: "b", Kind: "Deployment", Container: "main", Cluster: &a},
		{Name: "same", Namespace: "a", Kind: "Deployment", Container: "sidecar", Cluster: &a},
		{Name: "same", Namespace: "a", Kind: "StatefulSet", Container: "main", Cluster: &a},
		{Name: "same", Namespace: "a", Kind: "Deployment", Container: "main", Cluster: &b},
	}
	keys := map[string]bool{}
	for _, o := range objects {
		key := rowKey(o)
		if keys[key] {
			t.Fatalf("identity collision: %+v", o)
		}
		keys[key] = true
	}
}

func TestOOMKilledPodWithoutMetrics(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "memory-leak", Namespace: "keda", UID: "failed-pod", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "memory-leak", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("50Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("50Mi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "memory-leak", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-5 * time.Minute))}}}}},
	}
	for _, name := range []string{"simple", "simple_limit"} {
		cfg := config.Default(name)
		cfg.NamespaceValues, cfg.ResourceValues = []string{"keda"}, []string{"Pod"}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		loader := kube.NewLoader(cfg)
		clients := kube.Clients{Typed: fake.NewSimpleClientset(pod)}
		objects, err := loader.List(context.Background(), clients)
		if err != nil || len(objects) != 1 {
			t.Fatalf("discover failed standalone pod: %v %v", objects, err)
		}
		object, err := loader.Observe(context.Background(), clients, objects[0])
		if err != nil || object.CurrentPods() != 0 || object.DeletedPods() != 1 || len(object.OOMKills) != 1 {
			t.Fatalf("observe OOM: %+v %v", object, err)
		}
		metrics := strategy.Metrics{WindowStart: now.Add(-48 * time.Hour).UnixMilli(), EvaluationTime: time.Now().UnixMilli()}
		for _, tc := range []struct {
			enabled bool
			minimum int
			buffer  float64
			wantMi  float64
		}{{false, 100, 50, 0}, {true, 100, 50, 100}, {true, 10, 50, 75}, {true, 10, 25, 62.5}} {
			cfg.UseOOMKillData, cfg.MemoryMinValue, cfg.OOMMemoryBuffer = tc.enabled, tc.minimum, tc.buffer
			result, err := strategy.Run(cfg, metrics, object)
			if err != nil {
				t.Fatal(err)
			}
			scan := recommend.Scan(object, result)
			for _, value := range []model.MaybeValue{scan.Recommended.Requests[model.Memory].Value, scan.Recommended.Limits[model.Memory].Value} {
				if tc.enabled && (!value.Set || value.Unknown || value.Value != tc.wantMi*1024*1024) || !tc.enabled && !value.Unknown {
					t.Fatalf("%s, %+v: wrong OOM recommendation: %+v", name, tc, value)
				}
			}
			if !scan.Recommended.Requests[model.CPU].Value.Unknown {
				t.Fatal("OOM must not authorize CPU sizing without metrics")
			}
			text, err := report.Render(model.Report{Scans: []model.Scan{scan}}, cfg, true)
			if err != nil || strings.Contains(text, "Yes (5m ago)") != tc.enabled {
				t.Fatalf("OOM table: %s, %v", text, err)
			}
			if tc.enabled && tc.wantMi == 100 && !strings.Contains(text, "+50Mi") {
				t.Fatalf("failed OOM pod must show its per-container memory increase: %s", text)
			}
			if !tc.enabled {
				continue
			}
			store := runstore.Store{Root: t.TempDir()}
			saved, err := store.Save(runstore.Run{Strategy: name, Rows: []runstore.Row{runstore.NewRow(scan, metrics, runstore.Connection{OOM: true})}})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Load(saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			doc := explain.Build(loaded, loaded.Rows[0])
			if doc.Cards[2].Outcome != "recommended" || !strings.Contains(doc.Cards[2].Reason, "were not required") || !strings.Contains(doc.Cards[2].Reason, "current memory settings") {
				t.Fatalf("saved explanation lost OOM-only decision: %+v", doc.Cards[2])
			}
		}
	}
}

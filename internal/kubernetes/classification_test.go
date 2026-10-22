package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func classificationPod() corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-abc", Namespace: "demo", UID: "pod-uid", Labels: map[string]string{"app": "demo"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "api", Ports: []corev1.ContainerPort{{Name: "http-api", ContainerPort: 8080}}},
		{Name: "exporter", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9090}}},
	}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}
func classificationObject() model.Object {
	return model.Object{UID: "workload", Namespace: "demo", Name: "demo", Kind: "Deployment", Release: "release", Container: "api", Pods: []model.Pod{{Name: "demo-abc", UID: "pod-uid", Release: "release"}}}
}
func TestLeaseMatchesOwnedPodOrOwnerOnly(t *testing.T) {
	pod := classificationPod()
	object := classificationObject()
	holder := pod.Name + "_uuid"
	duration := int32(20)
	renewed := metav1.NewMicroTime(time.Now())
	lease := coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "election", Namespace: "demo"}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, RenewTime: &renewed}}
	if result, ok := attributedLease(lease, object, []corev1.Pod{pod}); !ok || result.PodUID != "pod-uid" {
		t.Fatalf("holder attribution: %+v %v", result, ok)
	}
	holder = "unrelated"
	if _, ok := attributedLease(lease, object, []corev1.Pod{pod}); ok {
		t.Fatal("namespace proximity matched unrelated holder")
	}
	lease.OwnerReferences = []metav1.OwnerReference{{UID: types.UID(object.UID)}}
	if _, ok := attributedLease(lease, object, []corev1.Pod{pod}); !ok {
		t.Fatal("verified owner ignored")
	}
	lease.Namespace = "other"
	if _, ok := attributedLease(lease, object, []corev1.Pod{pod}); ok {
		t.Fatal("cross-namespace lease matched")
	}
}
func TestEndpointSelectionAndContainerAttribution(t *testing.T) {
	pod := classificationPod()
	object := classificationObject()
	svc := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "demo"}, Ports: []corev1.ServicePort{{Name: "http-api", Port: 80, TargetPort: intstr.FromString("http-api")}, {Name: "metrics", Port: 9090, TargetPort: intstr.FromInt32(9090)}}}}
	candidates := endpointCandidates(object, []corev1.Pod{pod}, []corev1.Service{svc})
	httpCount := 0
	for _, c := range candidates {
		if c.observation.HTTP {
			httpCount++
			if c.observation.Container != "api" || c.observation.Port != 8080 {
				t.Fatalf("wrong API container: %+v", c)
			}
		}
	}
	if httpCount != 1 {
		t.Fatalf("HTTP candidates: %+v", candidates)
	}
	svc.Spec.Selector = map[string]string{"app": "unrelated"}
	for _, c := range endpointCandidates(object, []corev1.Pod{pod}, []corev1.Service{svc}) {
		if c.observation.HTTP {
			t.Fatal("unrelated service used")
		}
	}
	if c, _ := resolvePort(pod, "", 7777); c != "" {
		t.Fatal("ambiguous undeclared port assigned")
	}
	pod.Spec.Containers = pod.Spec.Containers[:1]
	if c, port := resolvePort(pod, "", 7777); c != "api" || port != 7777 {
		t.Fatal("single-container fallback failed")
	}
}
func TestHTTPHeuristicExcludesProbeAndMetricsPorts(t *testing.T) {
	for _, name := range []string{"http", "http-api", "rest", "https", "web", "api"} {
		if !httpListener(name, "") {
			t.Errorf("%s ignored", name)
		}
	}
	for _, name := range []string{"http-healthz", "health", "http-metrics", "ready", "probe", "tcp", "grpc"} {
		if httpListener(name, "") {
			t.Errorf("%s incorrectly HTTP", name)
		}
	}
	if !httpListener("port", "kubernetes.io/h2c") {
		t.Fatal("appProtocol ignored")
	}
}
func TestMetricsExpositionVerification(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		want                    bool
	}{
		{"prometheus", "# TYPE requests_total counter\nrequests_total{method=\"GET\"} 2\n", "text/plain;version=0.0.4", true},
		{"openmetrics", "# TYPE requests counter\nrequests_total 2 # {trace_id=\"abc\"} 1\n# EOF\n", "application/openmetrics-text;version=1.0.0", true},
		{"health", "ok\n", "text/plain", false},
		{"html", "<html>metrics 1</html>", "text/html", false},
		{"comments", "# HELP sample help\n", "text/plain", false},
		{"malformed", "sample 1\nbroken\n", "text/plain", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validMetrics([]byte(tc.body), tc.contentType); got != tc.want {
				t.Fatalf("validMetrics=%v", got)
			}
		})
	}
	for _, path := range []string{"//other/metrics", "/metrics?token=secret", "/../secrets", "/%2e%2e/secret", "/metrics#fragment"} {
		if safeMetricsPath(path) {
			t.Errorf("unsafe path accepted: %s", path)
		}
	}
	if !safeMetricsPath("/prometheus/metrics") {
		t.Fatal("custom path rejected")
	}
}
func TestClassificationProbeCachingAndServiceEvidence(t *testing.T) {
	pod := classificationPod()
	pod.Annotations = map[string]string{"prometheus.io/scrape": "true"}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "demo"}, Ports: []corev1.ServicePort{{Name: "http-api", Port: 80, TargetPort: intstr.FromInt32(8080)}}}}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.Contains(r.URL.Path, "/pods/http:demo-abc:") {
			t.Errorf("unexpected proxy path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, "requests_total 1")
	}))
	defer server.Close()
	clients := Clients{Typed: fake.NewClientset(svc), REST: &rest.Config{Host: server.URL}}
	loader := NewLoader(config.Default("simple"))
	object := classificationObject()
	var group sync.WaitGroup
	for range 5 {
		group.Go(func() {
			result := loader.classify(context.Background(), clients, object, []corev1.Pod{pod})
			httpFound := false
			for _, e := range result.Endpoints {
				if !e.MetricsVerified {
					t.Errorf("verification missing: %+v", e)
				}
				httpFound = httpFound || e.HTTP
			}
			if !httpFound {
				t.Error("Service HTTP evidence lost when probes deduplicated")
			}
		})
	}
	group.Wait()
	if requests.Load() != 2 {
		t.Fatalf("probes not deduplicated: %d", requests.Load())
	}
	for _, action := range clients.Typed.(*fake.Clientset).Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("unexpected action: %v", action)
		}
	}
}
func TestMetricsProbeFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		body, issue string
	}{
		{"forbidden", 403, "", "MetricsProbeDenied"},
		{"redirect", 302, "", "MetricsProbeHTTPFailure"},
		{"health-only", 200, "ok\n", "MetricsResponseInvalid"},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), "MetricsResponseTooLarge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://unrelated.invalid/metrics")
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			verified, issue := probeMetrics(context.Background(), Clients{REST: &rest.Config{Host: server.URL}}, "demo-abc", analysis.EndpointObservation{Namespace: "demo", Port: 8080, Path: "/metrics", Scheme: "http"})
			if verified || issue != tc.issue {
				t.Fatalf("probe: %v %s", verified, issue)
			}
		})
	}
}

func TestProbePreservesSharedHTTPClient(t *testing.T) {
	before := http.DefaultClient.CheckRedirect
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "requests_total 1") }))
	defer server.Close()
	ok, issue := probeMetrics(context.Background(), Clients{REST: &rest.Config{Host: server.URL}}, "demo", analysis.EndpointObservation{Namespace: "demo", Port: 8080, Path: "/metrics", Scheme: "http"})
	if !ok {
		t.Fatalf("probe failed: %s", issue)
	}
	if before == nil && http.DefaultClient.CheckRedirect != nil {
		t.Fatal("probe mutated shared HTTP client")
	}
}

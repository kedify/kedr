package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/kedify/kedr/internal/config"
)

func TestDeploymentDiscovery(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}}}}}}
	typed := fake.NewSimpleClientset(deployment)
	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cfg := config.Default("simple")
	cfg.NamespaceValues = []string{"default"}
	cfg.Namespaces = []string{"default"}
	cfg.ResourceValues = []string{"Deployment"}
	cfg.Resources = []string{"Deployment"}
	loader := NewLoader(cfg)
	name := "test"
	objects, err := loader.List(context.Background(), Clients{Name: &name, Typed: typed, Dynamic: dynamic})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[0].Kind != "Deployment" || objects[0].Selector != "app=api" {
		t.Fatalf("objects=%+v", objects)
	}
	data, err := json.Marshal(objects[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || objects[0].Pods == nil || objects[0].Labels == nil || objects[0].Annotations == nil {
		t.Fatalf("stable empty collections were not initialized: %s", data)
	}
}

func TestMetricsDiscoveryUsesOneInventoryPerKind(t *testing.T) {
	service := func(name string, selector map[string]string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "monitoring", Labels: selector}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 9090}}}}
	}
	prometheus := service("prom", map[string]string{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"})
	vm := service("vm", map[string]string{"app": "vmselect"})
	mimir := service("mimir", map[string]string{"app.kubernetes.io/name": "mimir", "app.kubernetes.io/component": "query-frontend"})
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "monitoring", Labels: map[string]string{"app": "vmselect"}}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "metrics.example"}}}}
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		inside  bool
		want    string
	}{
		{"late selector", []runtime.Object{prometheus}, false, "https://cluster/api/v1/namespaces/monitoring/services/prom:9090/proxy"},
		{"service priority", []runtime.Object{prometheus, vm}, false, "https://cluster/api/v1/namespaces/monitoring/services/vm:9090/proxy/select/0/prometheus"},
		{"ingress priority", []runtime.Object{prometheus, ingress}, false, "http://metrics.example"},
		{"service before matching ingress", []runtime.Object{vm, ingress}, false, "https://cluster/api/v1/namespaces/monitoring/services/vm:9090/proxy/select/0/prometheus"},
		{"mimir path", []runtime.Object{mimir}, false, "https://cluster/api/v1/namespaces/monitoring/services/mimir:9090/proxy/prometheus"},
		{"in cluster", []runtime.Object{prometheus, ingress}, true, "http://prom.monitoring.svc.cluster.local:9090"},
		{"missing", nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typed := fake.NewSimpleClientset(tc.objects...)
			name := "test"
			clients := Clients{Typed: typed, REST: &rest.Config{Host: "https://cluster"}}
			if !tc.inside {
				clients.Name = &name
			}
			got, err := DiscoverMetricsURL(context.Background(), clients)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
			counts := map[string]int{}
			for _, action := range typed.Actions() {
				if action.GetVerb() != "list" {
					t.Fatalf("unexpected API action: %+v", action)
				}
				counts[action.GetResource().Resource]++
			}
			if counts["services"] != 1 || counts["ingresses"] > 1 || tc.inside && counts["ingresses"] != 0 {
				t.Fatalf("redundant discovery calls: %v", counts)
			}
		})
	}
}

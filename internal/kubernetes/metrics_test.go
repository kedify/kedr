package kubernetes

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func TestMetricsDiscoveryDeduplicationAndIngress(t *testing.T) {
	labels := map[string]string{"app": "prometheus", "component": "server", "app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "prom", Namespace: "ns", Labels: labels}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "grpc", Port: 9095}, {Name: "http", Port: 9090}}}}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "ns", Labels: labels}, Spec: networkingv1.IngressSpec{
		TLS:   []networkingv1.IngressTLS{{Hosts: []string{"metrics.example"}}},
		Rules: []networkingv1.IngressRule{{Host: "metrics.example", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/prometheus/"}, {Path: "/prometheus"}}}}}},
	}}
	name := "test"
	clients := Clients{Name: &name, Typed: fake.NewSimpleClientset(service, ingress), REST: &rest.Config{Host: "https://cluster"}}
	got, err := DiscoverMetricsEndpoints(context.Background(), clients)
	if err != nil || len(got) != 2 || got[0].URL != "https://cluster/api/v1/namespaces/ns/services/prom:9090/proxy" || got[1].URL != "https://metrics.example/prometheus" {
		t.Fatalf("overlapping selectors, HTTP port, TLS or ingress paths: %+v, %v", got, err)
	}
	// Restricted ingress RBAC must not discard accessible services.
	clients.Typed.(*fake.Clientset).PrependReactor("list", "ingresses", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingresses"}, "", errors.New("forbidden"))
	})
	got, err = DiscoverMetricsEndpoints(context.Background(), clients)
	if err != nil || len(got) != 1 || got[0].Kind != "Service" {
		t.Fatalf("ingress RBAC blocked service discovery: %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverMetricsEndpoints(ctx, clients); !errors.Is(err, context.Canceled) {
		t.Fatalf("discovery ignored cancellation: %v", err)
	}
}

func TestMetricsProxyAuthenticationScope(t *testing.T) {
	for _, tc := range []struct {
		endpoint, api string
		want          bool
	}{
		{"https://cluster/api/v1/namespaces/ns/services/prom:9090/proxy", "https://cluster", true},
		{"https://cluster/api/v1/namespaces/ns/services/mimir:8080/proxy/prometheus", "https://cluster/", true},
		{"https://cluster/prefix/api/v1/namespaces/ns/services/prom:80/proxy", "https://cluster/prefix", true},
		{"https://metrics.example/prometheus", "https://cluster", false},
		{"https://cluster.evil/api/v1/namespaces/ns/services/prom:80/proxy", "https://cluster", false},
		{"http://cluster/api/v1/namespaces/ns/services/prom:80/proxy", "https://cluster", false},
		{"https://cluster/api/v1/namespaces/ns/services/prom:80/proxy-other", "https://cluster", false},
		{"https://cluster/api/v1/namespaces/ns/services/prom:80", "https://cluster", false},
		{"https://user@cluster/api/v1/namespaces/ns/services/prom:80/proxy", "https://cluster", false},
		{"https://cluster/api/v1/namespaces/ns/services/prom:80/proxy", "", false},
	} {
		if got := IsMetricsProxyURL(tc.endpoint, tc.api); got != tc.want {
			t.Errorf("endpoint=%q API=%q: got %t, want %t", tc.endpoint, tc.api, got, tc.want)
		}
	}
}

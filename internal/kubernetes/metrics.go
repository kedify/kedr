package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type MetricsEndpoint struct {
	URL, Backend, Namespace, Name, Kind string
}

// DiscoverMetricsEndpoints lists every matching endpoint, including alternatives
// from different backends. Overlapping label selectors never duplicate a URL.
func DiscoverMetricsEndpoints(ctx context.Context, clients Clients) ([]MetricsEndpoint, error) {
	selectors := []struct{ selector, backend, path string }{
		{"app.kubernetes.io/name=vmsingle", "VictoriaMetrics", ""},
		{"app.kubernetes.io/name=victoria-metrics-single", "VictoriaMetrics", ""},
		{"app.kubernetes.io/name=vmselect", "VictoriaMetrics", "/select/0/prometheus"},
		{"app.kubernetes.io/component=vmselect", "VictoriaMetrics", "/select/0/prometheus"},
		{"app=vmselect", "VictoriaMetrics", "/select/0/prometheus"},
		{"app.kubernetes.io/component=query,app.kubernetes.io/name=thanos", "Thanos", ""},
		{"app.kubernetes.io/name=thanos-query", "Thanos", ""},
		{"app=thanos-query", "Thanos", ""},
		{"app=thanos-querier", "Thanos", ""},
		{"app.kubernetes.io/name=mimir,app.kubernetes.io/component=query-frontend", "Mimir", "/prometheus"},
		{"app=kube-prometheus-stack-prometheus", "Prometheus", ""},
		{"app=prometheus,component=server", "Prometheus", ""},
		{"app=prometheus-server", "Prometheus", ""},
		{"app=prometheus-operator-prometheus", "Prometheus", ""},
		{"app=rancher-monitoring-prometheus", "Prometheus", ""},
		{"app=prometheus-prometheus", "Prometheus", ""},
		{"app.kubernetes.io/name=prometheus,app.kubernetes.io/component=server", "Prometheus", ""},
		{"app=stack-prometheus", "Prometheus", ""},
	}
	services, err := clients.Typed.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("discover metrics services: %w", err)
	}
	var ingresses *networkingv1.IngressList
	if clients.Name != nil {
		// Ingress discovery is optional: service-only RBAC remains sufficient.
		ingresses, _ = clients.Typed.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var endpoints []MetricsEndpoint
	add := func(endpoint MetricsEndpoint) {
		if !seen[endpoint.URL] {
			seen[endpoint.URL] = true
			endpoints = append(endpoints, endpoint)
		}
	}
	for _, selector := range selectors {
		matcher, err := labels.Parse(selector.selector)
		if err != nil {
			return nil, err
		}
		for _, svc := range services.Items {
			if !matcher.Matches(labels.Set(svc.Labels)) || len(svc.Spec.Ports) == 0 {
				continue
			}
			port := metricsServicePort(svc.Spec.Ports)
			endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, port)
			if clients.Name != nil {
				endpoint = fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s:%d/proxy", strings.TrimRight(clients.REST.Host, "/"), svc.Namespace, svc.Name, port)
			}
			add(MetricsEndpoint{URL: endpoint + selector.path, Backend: selector.backend, Namespace: svc.Namespace, Name: svc.Name, Kind: "Service"})
		}
		if ingresses == nil {
			continue
		}
		for _, ingress := range ingresses.Items {
			if !matcher.Matches(labels.Set(ingress.Labels)) {
				continue
			}
			for _, rule := range ingress.Spec.Rules {
				if rule.Host == "" {
					continue
				}
				scheme := "http"
				for _, tls := range ingress.Spec.TLS {
					if contains(tls.Hosts, rule.Host) {
						scheme = "https"
					}
				}
				paths := []string{""}
				if rule.HTTP != nil && len(rule.HTTP.Paths) > 0 {
					paths = nil
					for _, path := range rule.HTTP.Paths {
						paths = append(paths, path.Path)
					}
				}
				for _, path := range paths {
					path = strings.TrimRight(path, "/")
					if !strings.HasSuffix(path, selector.path) {
						path += selector.path
					}
					add(MetricsEndpoint{URL: scheme + "://" + rule.Host + path, Backend: selector.backend, Namespace: ingress.Namespace, Name: ingress.Name, Kind: "Ingress"})
				}
			}
		}
	}
	sort.Slice(endpoints, func(i, j int) bool {
		a, b := endpoints[i], endpoints[j]
		return a.Backend+"/"+a.Namespace+"/"+a.Name+"/"+a.Kind+"/"+a.URL < b.Backend+"/"+b.Namespace+"/"+b.Name+"/"+b.Kind+"/"+b.URL
	})
	if len(endpoints) == 0 {
		return nil, errors.New("prometheus-compatible service was not found; specify --prometheus-url")
	}
	return endpoints, nil
}

func metricsServicePort(ports []corev1.ServicePort) int32 {
	for _, port := range ports {
		if port.Name == "http" || port.Name == "web" || strings.HasPrefix(port.Name, "http-") {
			return port.Port
		}
	}
	return ports[0].Port
}

// IsMetricsProxyURL limits Kubernetes authentication to a service proxy on the
// selected API server, including URLs supplied explicitly with --prometheus-url.
func IsMetricsProxyURL(endpoint, apiServer string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil {
		return false
	}
	api, err := url.Parse(apiServer)
	if err != nil || api.Host == "" || u.Scheme != api.Scheme || u.Host != api.Host {
		return false
	}
	prefix := strings.TrimRight(api.Path, "/") + "/api/v1/namespaces/"
	if !strings.HasPrefix(u.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, prefix), "/")
	return len(parts) >= 4 && parts[0] != "" && parts[1] == "services" && parts[2] != "" && parts[3] == "proxy"
}

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
	"github.com/prometheus/prometheus/model/textparse"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type classificationNamespace struct {
	once            sync.Once
	services        []corev1.Service
	leases          []coordinationv1.Lease
	issues          []string
	observedAt      int64
	leasesAvailable bool
}
type classificationWorkload struct {
	once        sync.Once
	observation analysis.ClassificationObservation
}

func (l *Loader) classify(ctx context.Context, clients Clients, object model.Object, items []corev1.Pod) *analysis.ClassificationObservation {
	key := fmt.Sprintf("%p/%s/%s/%s", clients.Typed, object.Namespace, object.UID, object.Release)
	value, _ := l.classificationWorkloads.LoadOrStore(key, &classificationWorkload{})
	cached := value.(*classificationWorkload)
	cached.once.Do(func() {
		workCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cached.observation = l.discoverClassification(workCtx, clients, object, items)
	})
	return &cached.observation
}
func (l *Loader) classificationNamespace(ctx context.Context, clients Clients, namespace string) *classificationNamespace {
	key := fmt.Sprintf("%p/%s", clients.Typed, namespace)
	value, _ := l.classificationNamespaces.LoadOrStore(key, &classificationNamespace{})
	cached := value.(*classificationNamespace)
	cached.once.Do(func() {
		if services, err := clients.Typed.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{}); err == nil {
			cached.services = services.Items
		} else {
			cached.issues = append(cached.issues, "ServiceDiscoveryUnavailable")
		}
		if leases, err := clients.Typed.CoordinationV1().Leases(namespace).List(ctx, metav1.ListOptions{}); err == nil {
			cached.leasesAvailable = true
			cached.leases = leases.Items
			cached.observedAt = time.Now().UnixMilli()
		} else {
			cached.issues = append(cached.issues, "LeaseDiscoveryUnavailable")
		}
	})
	return cached
}
func (l *Loader) discoverClassification(ctx context.Context, clients Clients, object model.Object, items []corev1.Pod) analysis.ClassificationObservation {
	ns := l.classificationNamespace(ctx, clients, object.Namespace)
	out := analysis.ClassificationObservation{LeasesAvailable: ns.leasesAvailable, Issues: append([]string(nil), ns.issues...)}
	owned := map[string]bool{}
	for _, p := range object.Pods {
		if !p.Deleted && p.Release == object.Release {
			owned[p.UID] = true
		}
	}
	pods := make([]corev1.Pod, 0, len(owned))
	for _, p := range items {
		if owned[string(p.UID)] {
			pods = append(pods, p)
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for _, lease := range ns.leases {
		if observed, ok := attributedLease(lease, object, pods); ok {
			observed.ObservedAt = ns.observedAt
			out.Leases = append(out.Leases, observed)
		}
	}
	candidates := endpointCandidates(object, pods, ns.services)
	sort.Slice(candidates, func(i, j int) bool {
		// Hinted metrics endpoints are checked before unnamed ports.
		a, b := candidates[i], candidates[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.key() < b.key()
	})
	merged := make(map[string]endpointCandidate, len(candidates))
	order := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		key := candidate.key()
		if previous, ok := merged[key]; ok {
			previous.observation.HTTP = previous.observation.HTTP || candidate.observation.HTTP
			merged[key] = previous
		} else {
			merged[key] = candidate
			order = append(order, key)
		}
	}
	probes := 0
	for _, key := range order {
		candidate := merged[key]
		ep := candidate.observation
		if ep.Container == "" {
			ep.Issue = "UnresolvedContainerPort"
		} else if probes >= 16 || ctx.Err() != nil {
			ep.Issue = "MetricsProbeBudgetExceeded"
		} else {
			probes++
			ep.MetricsVerified, ep.Issue = probeMetrics(ctx, clients, candidate.pod, ep)
		}
		ep.Timestamp = time.Now().UnixMilli()
		out.Endpoints = append(out.Endpoints, ep)
	}
	return out
}
func attributedLease(lease coordinationv1.Lease, object model.Object, pods []corev1.Pod) (analysis.LeaseObservation, bool) {
	if lease.Namespace != object.Namespace || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return analysis.LeaseObservation{}, false
	}
	renewed := lease.Spec.RenewTime
	if renewed == nil {
		renewed = lease.Spec.AcquireTime
	}
	if renewed == nil {
		return analysis.LeaseObservation{}, false
	}
	matches := map[string]bool{}
	for _, pod := range pods {
		if lease.Spec.HolderIdentity != nil {
			holder := *lease.Spec.HolderIdentity
			if holder == pod.Name || holder == string(pod.UID) || strings.HasPrefix(holder, pod.Name+"_") {
				matches[string(pod.UID)] = true
			}
		}
		for _, owner := range lease.OwnerReferences {
			if string(owner.UID) == string(pod.UID) {
				matches[string(pod.UID)] = true
			}
		}
	}
	direct := false
	for _, owner := range lease.OwnerReferences {
		if string(owner.UID) == object.UID {
			direct = true
		}
	}
	if len(matches) != 1 && !direct {
		return analysis.LeaseObservation{}, false
	}
	podUID := ""
	if len(matches) == 1 {
		for uid := range matches {
			podUID = uid
		}
	}
	return analysis.LeaseObservation{Namespace: lease.Namespace, Name: lease.Name, WorkloadUID: object.UID, PodUID: podUID, Timestamp: renewed.UnixMilli(), ExpiresAt: renewed.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).UnixMilli()}, true
}

type endpointCandidate struct {
	observation analysis.EndpointObservation
	pod         string
	priority    int
}

func (c endpointCandidate) key() string {
	return fmt.Sprintf("%s/%d/%s/%s", c.observation.PodUID, c.observation.Port, c.observation.Scheme, c.observation.Path)
}
func endpointCandidates(object model.Object, pods []corev1.Pod, services []corev1.Service) []endpointCandidate {
	var out []endpointCandidate
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		add := func(name, container string, port int32, portName string, httpPort bool, annotations map[string]string) {
			if port < 1 || port > 65535 {
				return
			}
			scheme := "http"
			if port == 443 || strings.Contains(strings.ToLower(portName), "https") {
				scheme = "https"
			}
			if hint := annotations["prometheus.io/scheme"]; hint == "http" || hint == "https" {
				scheme = hint
			}
			paths := []string{"/metrics"}
			if hint := annotations["prometheus.io/path"]; safeMetricsPath(hint) && hint != "/metrics" {
				paths = append([]string{hint}, paths...)
			}
			priority := 2
			if strings.Contains(strings.ToLower(portName), "metric") || annotations["prometheus.io/scrape"] == "true" {
				priority = 0
			} else if httpPort {
				priority = 1
			}
			for _, path := range paths {
				out = append(out, endpointCandidate{pod: pod.Name, priority: priority, observation: analysis.EndpointObservation{Namespace: object.Namespace, Name: name, WorkloadUID: object.UID, Container: container, PodUID: string(pod.UID), Port: port, Path: path, Scheme: scheme, HTTP: httpPort}})
			}
		}
		for _, svc := range services {
			if svc.Namespace != pod.Namespace || len(svc.Spec.Selector) == 0 || svc.Spec.Type == corev1.ServiceTypeExternalName || !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)) {
				continue
			}
			for _, port := range svc.Spec.Ports {
				if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
					continue
				}
				target := port.TargetPort.IntVal
				if target == 0 && port.TargetPort.StrVal == "" {
					target = port.Port
				}
				container, resolved := resolvePort(pod, port.TargetPort.StrVal, target)
				appProtocol := ""
				if port.AppProtocol != nil {
					appProtocol = *port.AppProtocol
				}
				add("Service/"+svc.Name, container, resolved, port.Name, httpListener(port.Name, appProtocol), svc.Annotations)
			}
			if hint := svc.Annotations["prometheus.io/port"]; hint != "" {
				if port, err := strconv.ParseInt(hint, 10, 32); err == nil {
					container, resolved := resolvePort(pod, "", int32(port))
					add("Service/"+svc.Name, container, resolved, "metrics", false, svc.Annotations)
				}
			}
		}
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
					resolvedContainer, resolvedPort := resolvePort(pod, "", port.ContainerPort)
					add("Pod/"+pod.Name, resolvedContainer, resolvedPort, port.Name, false, pod.Annotations)
				}
			}
		}
		if hint := pod.Annotations["prometheus.io/port"]; hint != "" {
			if port, err := strconv.ParseInt(hint, 10, 32); err == nil {
				container, resolved := resolvePort(pod, "", int32(port))
				add("Pod/"+pod.Name, container, resolved, "metrics", false, pod.Annotations)
			}
		}
	}
	return out
}
func resolvePort(pod corev1.Pod, name string, number int32) (string, int32) {
	container := ""
	port := number
	matches := 0
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if (name != "" && p.Name == name || name == "" && p.ContainerPort == number) && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
				container = c.Name
				port = p.ContainerPort
				matches++
			}
		}
	}
	if matches == 1 {
		return container, port
	}
	if matches == 0 && name == "" && len(pod.Spec.Containers) == 1 {
		return pod.Spec.Containers[0].Name, number
	}
	return "", port
}
func httpListener(name, protocol string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' })
	for _, token := range tokens {
		switch token {
		case "health", "healthz", "ready", "readiness", "live", "liveness", "probe", "metrics", "metric":
			return false
		}
	}
	for _, token := range tokens {
		switch token {
		case "http", "https", "rest", "api", "web":
			return true
		}
	}
	switch strings.ToLower(protocol) {
	case "http", "https", "kubernetes.io/h2c", "kubernetes.io/ws", "kubernetes.io/wss":
		return true
	}
	return false
}
func safeMetricsPath(path string) bool {
	if len(path) == 0 || len(path) > 512 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n") {
		return false
	}
	u, err := url.Parse(path)
	return err == nil && u.RawQuery == "" && u.Fragment == "" && u.Host == "" && u.Path == path && !strings.Contains(path, "\\") && !pathTraversal(path)
}
func probeMetrics(ctx context.Context, clients Clients, pod string, ep analysis.EndpointObservation) (bool, string) {
	if clients.REST == nil {
		return false, "MetricsProxyUnavailable"
	}
	client, err := KubernetesHTTPClient(clients)
	if err != nil {
		return false, "MetricsProxyUnavailable"
	}
	// HTTPClientFor can return http.DefaultClient; never mutate a shared client.
	probeClient := *client
	probeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	proxy := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s:%s:%d/proxy%s", strings.TrimRight(clients.REST.Host, "/"), url.PathEscape(ep.Namespace), ep.Scheme, url.PathEscape(pod), ep.Port, ep.Path)
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, proxy, nil)
	if err != nil {
		return false, "InvalidMetricsProxy"
	}
	request.Header.Set("Accept", "application/openmetrics-text;version=1.0.0, text/plain;version=0.0.4;q=0.9")
	response, err := probeClient.Do(request)
	if err != nil {
		return false, "MetricsProbeUnreachable"
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized {
		return false, "MetricsProbeDenied"
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, "MetricsProbeHTTPFailure"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return false, "MetricsProbeReadFailed"
	}
	if len(data) > 1<<20 {
		return false, "MetricsResponseTooLarge"
	}
	if !validMetrics(data, response.Header.Get("Content-Type")) {
		return false, "MetricsResponseInvalid"
	}
	return true, ""
}
func validMetrics(data []byte, contentType string) bool {
	if contentType == "" {
		contentType = "text/plain"
	}
	parser, err := textparse.New(data, contentType, nil, textparse.ParserOptions{})
	if err != nil || parser == nil {
		return false
	}
	samples := 0
	for {
		entry, parseErr := parser.Next()
		if errors.Is(parseErr, io.EOF) {
			return samples > 0
		}
		if parseErr != nil {
			return false
		}
		if entry == textparse.EntrySeries || entry == textparse.EntryHistogram {
			samples++
		}
	}
}

func pathTraversal(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

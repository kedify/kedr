package app

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kedify/kedr/internal/model"
)

type diagnosticLogger interface {
	Warnf(string, ...any)
	Debugf(string, ...any)
}

type missingMetric struct {
	code, name, conflict string
	ownership            bool
}

func (m missingMetric) applies(object model.Object) bool {
	switch m.name {
	case "kube_replicaset_owner":
		return (object.Kind == "Deployment" || object.Kind == "Rollout") && (len(object.Pods) > 0 || len(object.Releases) > 0)
	case "kube_pod_owner":
		return slices.Contains([]string{"Deployment", "Rollout", "StatefulSet", "DaemonSet"}, object.Kind) && len(object.Pods) > 0
	default:
		return len(object.Pods) > 0
	}
}

func commonMissingMetrics(scans []model.Scan) []missingMetric {
	var missing []missingMetric
	for _, metric := range []missingMetric{
		{code: "HistoricalOwnershipUnavailable:kube_replicaset_owner", name: "kube_replicaset_owner", ownership: true},
		{code: "HistoricalOwnershipUnavailable:kube_pod_owner", name: "kube_pod_owner", ownership: true},
		{code: "NoPrometheusCPUUsage", name: "container_cpu_usage_seconds_total", conflict: "CPUUsageUnattributedSeries"},
		{code: "NoPrometheusMemoryUsage", name: "container_memory_working_set_bytes", conflict: "MemoryUsageUnattributedSeries"},
	} {
		eligible, absent := 0, 0
		for _, scan := range scans {
			if !metric.applies(scan.Object) {
				continue
			}
			eligible++
			warnings := scan.Object.Warnings
			// Rejected attribution is different from an empty metric response.
			// Query failures also lack the explicit missing-metric warning.
			if slices.Contains(warnings, metric.code) && (metric.conflict == "" || !slices.Contains(warnings, metric.conflict)) {
				absent++
			}
		}
		if eligible > 0 && absent == eligible {
			missing = append(missing, metric)
		}
	}
	return missing
}

// Summarize each metrics backend separately, preserving raw evidence in scans.
func logScanDiagnostics(log diagnosticLogger, scans []model.Scan, endpoints map[string]string) []map[string]any {
	byContext := make(map[string][]model.Scan)
	for _, scan := range scans {
		name := "in-cluster"
		if scan.Object.Cluster != nil {
			name = *scan.Object.Cluster
		}
		byContext[name] = append(byContext[name], scan)
	}
	contexts := make([]string, 0, len(byContext))
	for name := range byContext {
		contexts = append(contexts, name)
	}
	sort.Strings(contexts)
	var diagnostics []map[string]any
	for _, name := range contexts {
		group := byContext[name]
		missing := commonMissingMetrics(group)
		covered := make(map[string]bool, len(missing))
		if len(missing) > 0 {
			var ownership, usage, metrics []string
			for _, metric := range missing {
				covered[metric.code] = true
				metrics = append(metrics, metric.name)
				if metric.ownership {
					ownership = append(ownership, metric.name)
				} else {
					usage = append(usage, metric.name)
				}
			}
			lines := []string{fmt.Sprintf("Required Kubernetes metrics are missing for all applicable scanned workloads in context %q.", name)}
			if endpoint := endpoints[name]; endpoint != "" {
				lines = append(lines, "Prometheus URL: "+endpoint)
			}
			if len(ownership) > 0 {
				lines = append(lines, "Missing KSM ownership metrics: "+strings.Join(ownership, ", ")+". Ensure kube-state-metrics is installed and running, and configure Prometheus to scrape it.")
			}
			if len(usage) > 0 {
				lines = append(lines, "Missing container usage metrics: "+strings.Join(usage, ", ")+". Configure Prometheus to scrape kubelet/cAdvisor for CPU and memory usage; kube-state-metrics does not provide these metrics.")
			}
			lines = append(lines, "If using Mimir, ensure the scraper remote-writes these metrics to the queried tenant. Check the selected endpoint, tenant, and cluster/namespace filters; missing results do not necessarily mean the exporters are absent.")
			message := strings.Join(lines, "\n")
			log.Warnf("Warning: %s", message)
			diagnostics = append(diagnostics, map[string]any{"name": "RequiredMetricsMissing", "context": name, "endpoint": endpoints[name], "metrics": metrics, "message": message})
		}
		for _, scan := range group {
			var remaining []string
			for _, warning := range scan.Object.Warnings {
				if !covered[warning] {
					remaining = append(remaining, warning)
				}
			}
			if len(remaining) > 0 {
				log.Debugf("Diagnostics for %s/%s/%s: %s", scan.Object.Namespace, scan.Object.Name, scan.Object.Container, strings.Join(remaining, "; "))
			}
		}
	}
	return diagnostics
}

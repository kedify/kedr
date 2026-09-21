package prometheus

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

func clusterMatcher(cfg *config.Config) string {
	if cfg.PrometheusClusterLabel == nil || cfg.PrometheusLabel == nil {
		return ""
	}
	return fmt.Sprintf(`, %s=%q`, *cfg.PrometheusLabel, *cfg.PrometheusClusterLabel)
}

func podRegex(object model.Object) string {
	parts := make([]string, 0, len(object.Pods))
	for _, pod := range object.Pods {
		parts = append(parts, regexp.QuoteMeta(pod.Name))
	}
	return strings.Join(parts, "|")
}

func BuildMetricQuery(metric string, object model.Object, cfg *config.Config) string {
	labels := fmt.Sprintf(`namespace=%q,pod=~%q,container=%q%s`, object.Namespace, podRegex(object), object.Container, clusterMatcher(cfg))
	switch metric {
	case "CPUUsage":
		return fmt.Sprintf(`container_cpu_usage_seconds_total{cpu=~"total|",%s}`, labels)
	case "MemoryUsage":
		return fmt.Sprintf(`container_memory_working_set_bytes{%s}`, labels)
	case "OOMKilledTimestamp":
		return fmt.Sprintf(`kube_pod_container_status_last_terminated_timestamp{%s} * ignoring(reason) (kube_pod_container_status_last_terminated_reason{reason="OOMKilled",%s} == 1)`, labels, labels)
	default:
		return ""
	}
}

func (c *Client) HistoryRange(ctx context.Context) (time.Time, time.Time, error) {
	now := time.Now()
	rows, err := c.QueryRange(ctx, "max(prometheus_tsdb_head_series)", now.Add(-5*time.Hour), now, time.Hour)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if len(rows) == 0 || len(rows[0].Values) == 0 {
		return time.Time{}, time.Time{}, errors.New("history range unavailable")
	}
	first, e1 := parsePair(rows[0].Values[0])
	last, e2 := parsePair(rows[0].Values[len(rows[0].Values)-1])
	if e1 != nil {
		return time.Time{}, time.Time{}, e1
	}
	if e2 != nil {
		return time.Time{}, time.Time{}, e2
	}
	return time.Unix(int64(first.Time), 0), time.Unix(int64(last.Time), 0), nil
}

func (c *Client) ClusterSummary(ctx context.Context) map[string]any {
	cluster := strings.TrimPrefix(clusterMatcher(c.cfg), ", ")
	queries := map[string]string{
		"cluster_memory":      fmt.Sprintf(`sum(max by (instance) (machine_memory_bytes{%s}))`, cluster),
		"cluster_cpu":         fmt.Sprintf(`sum(max by (instance) (machine_cpu_cores{%s}))`, cluster),
		"kube_system_mem_req": fmt.Sprintf(`sum(max(kube_pod_container_resource_requests{namespace="kube-system",resource="memory"%s}) by (job,pod,container))`, clusterMatcher(c.cfg)),
		"kube_system_cpu_req": fmt.Sprintf(`sum(max(kube_pod_container_resource_requests{namespace="kube-system",resource="cpu"%s}) by (job,pod,container))`, clusterMatcher(c.cfg)),
	}
	result := map[string]any{}
	for key, q := range queries {
		rows, err := c.Query(ctx, q)
		if err != nil || len(rows) != 1 || len(rows[0].Value) != 2 {
			return map[string]any{}
		}
		point, err := parsePair(rows[0].Value)
		if err != nil {
			return map[string]any{}
		}
		result[key] = point.Value
	}
	return result
}

func MetricsForStrategy(cfg *config.Config) []string {
	names := []string{"CPUUsage", "MemoryUsage"}
	if cfg.UseOOMKillData {
		names = append(names, "OOMKilledTimestamp")
	}
	return names
}

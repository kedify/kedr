package prometheus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
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

func durationString(d time.Duration) string {
	if d > 24*time.Hour {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m"
}

func podRegex(object model.Object) string {
	parts := make([]string, 0, len(object.Pods))
	for _, pod := range object.Pods {
		parts = append(parts, regexp.QuoteMeta(pod.Name))
	}
	return strings.Join(parts, "|")
}

func BuildMetricQuery(metric string, object model.Object, cfg *config.Config) string {
	pods, cluster := podRegex(object), clusterMatcher(cfg)
	duration, step := durationString(history(cfg)), strconv.Itoa(int(timeframe(cfg).Seconds()))+"s"
	labels := fmt.Sprintf(`namespace=%q,pod=~%q,container=%q%s`, object.Namespace, pods, object.Container, cluster)
	switch metric {
	case "CPULoader":
		return fmt.Sprintf(`max(rate(container_cpu_usage_seconds_total{%s}[%s])) by (container, pod, job)`, labels, step)
	case "PercentileCPULoader":
		return fmt.Sprintf(`quantile_over_time(%.2g,max(rate(container_cpu_usage_seconds_total{%s}[%s])) by (container, pod, job)[%s:%s])`, cfg.CPUPercentile/100, labels, step, duration, step)
	case "CPUAmountLoader":
		return fmt.Sprintf(`count_over_time(max(container_cpu_usage_seconds_total{%s}) by (container, pod, job)[%s:%s])`, labels, duration, step)
	case "MaxMemoryLoader":
		return fmt.Sprintf(`max_over_time(max(container_memory_working_set_bytes{%s}) by (container, pod, job)[%s:%s])`, labels, duration, step)
	case "MemoryAmountLoader":
		return fmt.Sprintf(`count_over_time(max(container_memory_working_set_bytes{%s}) by (container, pod, job)[%s:%s])`, labels, duration, step)
	case "MaxOOMKilledMemoryLoader":
		limits := `resource="memory",` + labels
		reason := `reason="OOMKilled",` + labels
		return fmt.Sprintf(`max_over_time(max(max(kube_pod_container_resource_limits{%s}) by (pod, container, job) * on(pod, container, job) group_left(reason) max(kube_pod_container_status_last_terminated_reason{%s}) by (pod, container, job, reason)) by (container, pod, job)[%s:%s])`, limits, reason, duration, step)
	default:
		return ""
	}
}

func (c *Client) LoadPods(ctx context.Context, object model.Object) ([]model.Pod, error) {
	period := history(c.cfg)
	literal := ""
	if period <= 24*time.Hour {
		hours := int(period / time.Hour)
		if hours > 32 {
			hours = 32
		}
		literal = fmt.Sprintf("%dh", hours)
	} else {
		days := int(period / (24 * time.Hour))
		if days > 32 {
			days = 32
		}
		literal = fmt.Sprintf("%dd", days)
	}
	owners, ownerKind := []string{object.Name}, object.Kind
	cluster := clusterMatcher(c.cfg)
	switch object.Kind {
	case "Deployment", "Rollout":
		q := fmt.Sprintf(`kube_replicaset_owner{owner_name=%q,owner_kind=%q,namespace=%q%s}[%s]`, object.Name, object.Kind, object.Namespace, cluster, literal)
		rows, err := c.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		owners = owners[:0]
		for _, row := range rows {
			if name := row.Metric["replicaset"]; name != "" {
				owners = append(owners, name)
			}
		}
		ownerKind = "ReplicaSet"
	case "CronJob":
		q := fmt.Sprintf(`kube_job_owner{owner_name=%q,owner_kind="CronJob",namespace=%q%s}[%s]`, object.Name, object.Namespace, cluster, literal)
		rows, err := c.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		owners = owners[:0]
		for _, row := range rows {
			if name := row.Metric["job_name"]; name != "" {
				owners = append(owners, name)
			}
		}
		ownerKind = "Job"
	case "GroupedJob":
		owners, ownerKind = object.GroupedJobs, "Job"
	default:
	}
	if len(owners) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	ownerBatchSize := 100
	if raw := os.Getenv("KRR_OWNER_BATCH_SIZE"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			ownerBatchSize = parsed
		}
	}
	for start := 0; start < len(owners); start += ownerBatchSize {
		end := start + ownerBatchSize
		if end > len(owners) {
			end = len(owners)
		}
		quoted := make([]string, 0, end-start)
		for _, owner := range owners[start:end] {
			quoted = append(quoted, regexp.QuoteMeta(owner))
		}
		q := fmt.Sprintf(`last_over_time(kube_pod_owner{owner_name=~%q,owner_kind=%q,namespace=%q%s}[%s])`, strings.Join(quoted, "|"), ownerKind, object.Namespace, cluster, literal)
		rows, err := c.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if name := row.Metric[relatedPodLabel()]; name != "" {
				seen[name] = false
			}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, regexp.QuoteMeta(name))
	}
	sort.Strings(names)
	for start := 0; start < len(names); start += 100 {
		end := start + 100
		if end > len(names) {
			end = len(names)
		}
		q := fmt.Sprintf(`kube_pod_status_phase{phase="Running",%s=~%q,namespace=%q%s} == 1`, relatedPodLabel(), strings.Join(names[start:end], "|"), object.Namespace, cluster)
		rows, err := c.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			seen[row.Metric[relatedPodLabel()]] = true
		}
	}
	pods := make([]model.Pod, 0, len(seen))
	for name, running := range seen {
		pods = append(pods, model.Pod{Name: name, Deleted: !running})
	}
	return pods, nil
}

func relatedPodLabel() string {
	if value := strings.TrimSpace(os.Getenv("KRR_RELATED_POD_LABEL")); value != "" {
		return value
	}
	return "pod"
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
	names := []string{"MaxMemoryLoader", "CPUAmountLoader", "MemoryAmountLoader"}
	if cfg.Strategy == "simple" {
		names = append(names, "PercentileCPULoader")
	} else {
		names = append(names, "CPULoader")
	}
	if cfg.UseOOMKillData {
		names = append(names, "MaxOOMKilledMemoryLoader")
	}
	return names
}

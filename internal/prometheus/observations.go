package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
	"golang.org/x/sync/errgroup"
)

type sampledSeries struct {
	labels  map[string]string
	samples []analysis.Sample
}

func (c *Client) Gather(ctx context.Context, object model.Object) (strategy.Metrics, []string) {
	end := time.Now().UTC()
	start := end.Add(-history(c.cfg))
	return c.GatherWindow(ctx, object, start, end)
}

// GatherWindow collects native observations for an immutable saved interval.
func (c *Client) GatherWindow(ctx context.Context, object model.Object, start, end time.Time) (strategy.Metrics, []string) {
	result := strategy.Metrics{ClassifyWorkloads: c.cfg.ClassifyWorkloads, WindowStart: start.UnixMilli(), EvaluationTime: end.UnixMilli()}
	var warnings []string
	live := livePodsByName(object)
	verified := make(map[string][]analysis.Series, len(cfsFamilies))
	for _, metric := range MetricsForStrategy(c.cfg) {
		rows, err := c.loadMetric(ctx, metric, object, start, end)
		if err != nil {
			if c.logger != nil {
				c.logger.Warnf("failed to gather %s for %s/%s: %v", metric, object.Namespace, object.Name, err)
			}
			warnings = append(warnings, metric+"CollectionFailed")
			continue
		}
		if metric == "OOMKilledTimestamp" {
			result.OOMKills = convertOOMEvents(rows, object, result.WindowStart, result.EvaluationTime)
			continue // No positive events is not a memory-usage collection failure.
		}
		kind := analysis.SampleGauge
		if metric == "CPUUsage" {
			kind = analysis.SampleCPUCounterSeconds
		}
		data, audit := convertUsageAudited(rows, object, kind)
		if audit.excluded {
			warnings = append(warnings, metric+"UnattributedSeries")
		}
		if len(data) == 0 {
			warnings = append(warnings, "NoPrometheus"+metric)
		}
		switch metric {
		case "CPUUsage":
			result.CPU = data
		case "MemoryUsage":
			result.Memory = data
		case "CPUThrottledSeconds":
			result.Throttling.Seconds = data
		case "CPUThrottledPeriods":
			result.Throttling.ThrottledPeriods = data
		case "CPUPeriods":
			result.Throttling.Periods = data
		}
		if len(live) > 0 && (slices.Contains(cfsFamilies, metric) || metric == "CPUUsage" && c.cfg.Strategy == "simple_limit") {
			verified[metric] = currentRuntimeSeries(rows, object, live, kind)
		}
	}
	// Keep earlier rollouts for the guard's segment boundary. If current
	// coverage is proved, only current-runtime rows can represent this release.
	complete := len(live) > 0
	if c.cfg.Strategy == "simple_limit" {
		complete = complete && len(verified["CPUUsage"]) == len(live)
	}
	for _, family := range cfsFamilies {
		complete = complete && len(verified[family]) == len(live)
	}
	if complete {
		result.Throttling.Seconds = withCurrentCounters(result.Throttling.Seconds, verified["CPUThrottledSeconds"], object.Release)
		result.Throttling.ThrottledPeriods = withCurrentCounters(result.Throttling.ThrottledPeriods, verified["CPUThrottledPeriods"], object.Release)
		result.Throttling.Periods = withCurrentCounters(result.Throttling.Periods, verified["CPUPeriods"], object.Release)
	}
	result.Throttling.SourceComplete = complete
	return result, warnings
}

func (c *Client) loadMetric(ctx context.Context, metric string, object model.Object, start, end time.Time) ([]sampledSeries, error) {
	merged := make(map[string]sampledSeries)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(c.cfg.MaxWorkers)
	var mu sync.Mutex
	// Keep the original response budget (50 pods × 6 hours), but let small
	// workloads fetch longer windows instead of paying for mostly empty queries.
	names := make(map[string]struct{}, len(object.Pods))
	for _, pod := range object.Pods {
		names[pod.Name] = struct{}{}
	}
	chunkDuration := 6 * time.Hour
	if metric != "OOMKilledTimestamp" && len(names) > 0 && len(names) < 50 {
		chunkDuration = chunkDuration * 50 / time.Duration(len(names))
		// Prometheus range selectors and evaluation times use milliseconds.
		chunkDuration = chunkDuration.Truncate(time.Millisecond)
	}
	// Schedule time chunks as well as pod batches. Otherwise a small workload
	// serializes an entire lookback (112 requests for two metrics over 14 days).
	// The client's query semaphore also bounds requests across all workloads.
	for chunkStart := start; chunkStart.Before(end); chunkStart = chunkStart.Add(chunkDuration) {
		chunkEnd := minTime(chunkStart.Add(chunkDuration), end)
		var pods []model.Pod
		seen := make(map[string]bool)
		for _, pod := range object.Pods {
			// Usage outside a verified pod lifetime is discarded by convertUsage.
			// OOM values can describe earlier events, so retain their whole window.
			if metric != "OOMKilledTimestamp" && (pod.CreatedAt > chunkEnd.UnixMilli() || pod.EndedAt > 0 && pod.EndedAt <= chunkStart.UnixMilli()) {
				continue
			}
			if !seen[pod.Name] {
				pods = append(pods, pod)
				seen[pod.Name] = true
			}
		}
		for first := 0; first < len(pods); first += 50 {
			if groupCtx.Err() != nil {
				break
			}
			batch := object
			batch.Pods = pods[first:min(first+50, len(pods))]
			group.Go(func() error {
				query := BuildMetricQuery(metric, batch, c.cfg)
				var samples []sampledSeries
				var err error
				if metric == "OOMKilledTimestamp" {
					var values []series
					values, err = c.QueryRange(groupCtx, query, chunkStart, chunkEnd, timeframe(c.cfg))
					if err == nil {
						samples, err = eventSamples(values)
					}
				} else {
					var values []series
					// Range selectors are left-open. Adjacent chunks therefore neither
					// drop nor duplicate their shared boundary sample.
					values, err = c.QuerySamples(groupCtx, query, chunkStart, chunkEnd)
					if err == nil {
						samples, err = nativeSamples(values)
					}
				}
				if err != nil {
					return err
				}
				mu.Lock()
				// Merge as each response finishes instead of retaining a second
				// complete copy of the unmerged history until all queries finish.
				for _, row := range samples {
					key := labelsKey(row.labels)
					item, exists := merged[key]
					if !exists {
						merged[key] = row
						continue
					}
					item.samples = append(item.samples, row.samples...)
					merged[key] = item
				}
				mu.Unlock()
				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]sampledSeries, 0, len(merged))
	for _, row := range merged {
		// Responses can finish out of order. Keep duplicate source timestamps
		// for the analyzer to validate, while restoring chronological order.
		sort.SliceStable(row.samples, func(i, j int) bool { return row.samples[i].Timestamp < row.samples[j].Timestamp })
		out = append(out, row)
	}
	return out, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func nativeSamples(rows []series) ([]sampledSeries, error) {
	out := make([]sampledSeries, 0, len(rows))
	for _, row := range rows {
		result := sampledSeries{labels: row.Metric, samples: make([]analysis.Sample, 0, len(row.Values))}
		for _, p := range row.Values {
			if math.IsNaN(p.Time) || math.IsInf(p.Time, 0) || p.Time <= 0 || p.Time >= float64(math.MaxInt64)/1000 || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 {
				return nil, errors.New("invalid native usage sample")
			}
			result.samples = append(result.samples, analysis.Sample{Timestamp: int64(math.Round(p.Time * 1000)), Value: p.Value})
		}
		out = append(out, result)
	}
	return out, nil
}

func labelsKey(labels map[string]string) string {
	copy := make(map[string]string, len(labels))
	for k, v := range labels {
		if k != "__name__" {
			copy[k] = v
		}
	}
	data, _ := json.Marshal(copy) // encoding/json sorts map keys.
	return string(data)
}

func eventSamples(rows []series) ([]sampledSeries, error) {
	out := make([]sampledSeries, 0, len(rows))
	for _, row := range rows {
		result := sampledSeries{labels: row.Metric, samples: make([]analysis.Sample, 0, len(row.Values))}
		for _, p := range row.Values {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 || p.Value >= float64(math.MaxInt64)/1000 {
				return nil, errors.New("invalid OOM event time")
			}
			// The value, not query evaluation time, is the actual termination time.
			result.samples = append(result.samples, analysis.Sample{Timestamp: int64(math.Round(p.Value * 1000))})
		}
		out = append(out, result)
	}
	return out, nil
}

// Scrape replicas are selected deterministically; container lifetimes are never
// joined by pod name. Cadvisor IDs distinguish past restarts within a pod UID.
func convertUsage(rows []sampledSeries, object model.Object, kind analysis.SampleKind) ([]analysis.Series, bool) {
	out, audit := convertUsageAudited(rows, object, kind)
	return out, audit.excluded
}

var cfsFamilies = []string{"CPUThrottledSeconds", "CPUThrottledPeriods", "CPUPeriods"}

// usageAudit records why a converted source cannot be certified complete.
type usageAudit struct {
	excluded   bool // unattributed, foreign-namespace or unmatched rows
	inferredID bool // lifetime taken from pod status, not from the scraped series
	duplicate  bool // several scraped rows resolved to the same container lifetime
	ambiguous  bool // a row without a pod UID matched several pods of that name
}

// livePodsByName scopes certification to currently running containers with a
// known runtime and start. An unidentifiable or repeated Pod leaves coverage unknown.
func livePodsByName(object model.Object) map[string]model.Pod {
	live := make(map[string]model.Pod)
	uids := make(map[string]bool)
	if object.Release == "" {
		return nil
	}
	for _, pod := range object.Pods {
		if pod.Deleted || pod.EndedAt != 0 || pod.Release != object.Release {
			continue
		}
		if pod.Name == "" || pod.UID == "" || uids[pod.UID] ||
			pod.ContainerID == "" || pod.ContainerStartedAt <= 0 || live[pod.Name].UID != "" {
			return nil
		}
		live[pod.Name] = pod
		uids[pod.UID] = true
	}
	return live
}

func currentRuntimeSeries(rows []sampledSeries, object model.Object, live map[string]model.Pod, kind analysis.SampleKind) []analysis.Series {
	selected := make([]sampledSeries, 0, len(live))
	seen := make(map[string]bool)
	for _, row := range rows {
		pod, found := live[row.labels["pod"]]
		if !found || row.labels["namespace"] != object.Namespace || row.labels["container"] != object.Container ||
			(row.labels["uid"] != "" && row.labels["uid"] != pod.UID) {
			continue
		}
		id := row.labels["id"]
		if id == "" {
			id = row.labels["container_id"]
		}
		_, runningID, ok := strings.Cut(pod.ContainerID, "://")
		if !ok || runningID == "" || normalizeRuntimeID(id) != runningID {
			continue
		}
		if seen[pod.UID] {
			return nil
		}
		var samples []analysis.Sample
		for _, sample := range row.samples {
			if sample.Timestamp >= max(pod.CreatedAt, pod.ContainerStartedAt) {
				samples = append(samples, sample)
			}
		}
		if len(samples) > 0 {
			seen[pod.UID] = true
			// Match counter families even if their exporters spell the same
			// runtime identifier differently.
			selected = append(selected, sampledSeries{
				labels:  map[string]string{"pod": pod.Name, "uid": pod.UID, "namespace": object.Namespace, "id": runningID},
				samples: samples,
			})
		}
	}
	data, audit := convertUsageAudited(selected, object, kind)
	if audit.excluded || audit.inferredID || audit.duplicate || audit.ambiguous {
		return nil
	}
	return data
}

func normalizeRuntimeID(id string) string {
	if _, suffix, ok := strings.Cut(id, "://"); ok {
		return suffix
	}
	id = strings.TrimRight(id, "/")
	id = id[strings.LastIndexByte(id, '/')+1:]
	if scope, ok := strings.CutSuffix(id, ".scope"); ok {
		for _, prefix := range []string{"cri-containerd-", "crio-", "docker-"} {
			if id, ok := strings.CutPrefix(scope, prefix); ok {
				return id
			}
		}
		return ""
	}
	return id
}

func withCurrentCounters(history, current []analysis.Series, release string) []analysis.Series {
	for _, series := range history {
		if series.Release != release {
			current = append(current, series)
		}
	}
	return current
}

// convertUsageAudited is convertUsage plus the provenance needed to decide
// whether a raw CFS source may claim complete coverage.
func convertUsageAudited(rows []sampledSeries, object model.Object, kind analysis.SampleKind) ([]analysis.Series, usageAudit) {
	pods := make(map[string][]model.Pod, len(object.Pods))
	for _, pod := range object.Pods {
		pods[pod.Name] = append(pods[pod.Name], pod)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].labels, rows[j].labels
		if (a["job"] == "kubelet") != (b["job"] == "kubelet") {
			return a["job"] == "kubelet"
		}
		return labelsKey(a) < labelsKey(b)
	})
	seen := map[string]bool{}
	var out []analysis.Series
	var audit usageAudit
	for _, row := range rows {
		candidates := pods[row.labels["pod"]]
		if len(candidates) == 0 {
			audit.excluded = true
			continue
		}
		if ns := row.labels["namespace"]; ns != "" && ns != object.Namespace {
			audit.excluded = true
			continue
		}
		matched := 0
		for _, pod := range candidates {
			if pod.UID == "" || pod.Release == "" || pod.CreatedAt <= 0 || row.labels["uid"] != "" && row.labels["uid"] != pod.UID {
				continue
			}
			matched++
			lifetime := row.labels["id"]
			if lifetime == "" {
				lifetime = row.labels["container_id"]
			}
			lowerBound := pod.CreatedAt
			if lifetime == "" {
				if pod.ContainerID == "" || pod.ContainerStartedAt <= 0 {
					audit.excluded = true
					continue
				}
				// Continuity of an unlabeled scraped series with this runtime ID is
				// not proven; usable for sizing, never for certifying completeness.
				audit.inferredID = true
				lifetime, lowerBound = pod.ContainerID, max(lowerBound, pod.ContainerStartedAt)
			}
			id := pod.UID + "/" + object.Container + "/" + lifetime
			if seen[id] {
				audit.duplicate = true
				continue
			}
			s := analysis.Series{ID: id, PodUID: pod.UID, WorkloadUID: object.UID, Release: pod.Release, Kind: kind}
			for _, sample := range row.samples {
				if sample.Timestamp >= lowerBound && (pod.EndedAt == 0 || sample.Timestamp < pod.EndedAt) {
					s.Samples = append(s.Samples, sample)
				}
			}
			if len(s.Samples) == 0 {
				continue
			}
			seen[id] = true
			out = append(out, s)
		}
		if matched == 0 {
			audit.excluded = true
		} else if matched > 1 && row.labels["uid"] == "" {
			audit.ambiguous = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, audit
}

func convertOOMEvents(rows []sampledSeries, object model.Object, start, end int64) []analysis.OOMKill {
	pods := make(map[string][]model.Pod, len(object.Pods))
	for _, pod := range object.Pods {
		pods[pod.Name] = append(pods[pod.Name], pod)
	}
	seen := map[string]bool{}
	var kills []analysis.OOMKill
	for _, row := range rows {
		if ns := row.labels["namespace"]; ns != "" && ns != object.Namespace {
			continue
		}
		for _, pod := range pods[row.labels["pod"]] {
			if pod.UID == "" || pod.Release == "" || pod.CreatedAt <= 0 || row.labels["uid"] != "" && row.labels["uid"] != pod.UID {
				continue
			}
			for _, sample := range row.samples {
				ts := sample.Timestamp
				if ts < max(start, pod.CreatedAt) || ts > end || pod.EndedAt > 0 && ts >= pod.EndedAt {
					continue
				}
				id := strings.Join([]string{pod.UID, object.Container, strconv.FormatInt(ts, 10)}, "/")
				if seen[id] {
					continue
				}
				seen[id] = true
				// Do not pretend the current limit was the limit at termination.
				kills = append(kills, analysis.OOMKill{ID: id, PodUID: pod.UID, WorkloadUID: object.UID, Release: pod.Release, Timestamp: ts})
			}
		}
	}
	sort.Slice(kills, func(i, j int) bool { return kills[i].ID < kills[j].ID })
	return kills
}

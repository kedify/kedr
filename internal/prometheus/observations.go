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
		scope := object
		isQuota := metric == "CPUQuota" || metric == "CPUPeriod"
		if isQuota {
			limit := object.Allocations.Limits[model.CPU]
			if len(live) == 0 || !limit.Set || limit.Unknown {
				continue
			}
			scope.Pods = make([]model.Pod, 0, len(live))
			for _, pod := range object.Pods {
				if current, ok := live[pod.Name]; ok && current.UID == pod.UID {
					scope.Pods = append(scope.Pods, pod)
				}
			}
		}
		rows, err := c.loadMetric(ctx, metric, scope, start, end)
		if err != nil {
			if c.logger != nil {
				c.logger.Warnf("failed to gather %s for %s/%s: %v", metric, object.Namespace, object.Name, err)
			}
			warnings = append(warnings, metric+"CollectionFailed")
			if metric == "OOMKilledTimestamp" {
				result.OOMCoverageIncomplete = true
			}
			continue
		}
		if metric == "OOMKilledTimestamp" {
			result.OOMKills, result.OOMCoverageIncomplete = convertOOMEvents(rows, object, result.WindowStart, result.EvaluationTime)
			continue // No positive events is not a memory-usage collection failure.
		}
		kind := analysis.SampleGauge
		if metric == "CPUUsage" {
			kind = analysis.SampleCPUCounterSeconds
		}
		if isQuota {
			verified[metric] = currentRuntimeSeries(rows, object, live, kind)
			if len(verified[metric]) != len(live) {
				warnings = append(warnings, metric+"CurrentRuntimeUnavailable")
			}
			continue
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
	// Use verified current-runtime samples for proof. Preserve history from
	// earlier runtimes for pressure evidence and rollback boundaries.
	complete := len(live) > 0
	if c.cfg.Strategy == "simple_limit" {
		complete = complete && len(verified["CPUUsage"]) == len(live)
		result.CPU = withCurrentRuntime(result.CPU, verified["CPUUsage"], object.Container, object.Release)
		limit := object.Allocations.Limits[model.CPU]
		if limit.Set && !limit.Unknown {
			quotaUnverified := false
			for _, family := range cfsFamilies {
				before := len(verified[family])
				verified[family] = atAppliedQuota(verified[family], verified["CPUQuota"], verified["CPUPeriod"], limit.Value)
				quotaUnverified = quotaUnverified || before > len(verified[family])
			}
			if quotaUnverified {
				warnings = append(warnings, "CPUQuotaHistoryUnverified")
			}
		}
	}
	for _, family := range cfsFamilies {
		complete = complete && len(verified[family]) == len(live)
	}
	if complete && c.cfg.Strategy == "simple_limit" {
		result.Throttling.Seconds = withCurrentRuntime(result.Throttling.Seconds, verified["CPUThrottledSeconds"], object.Container, object.Release)
		result.Throttling.ThrottledPeriods = withCurrentRuntime(result.Throttling.ThrottledPeriods, verified["CPUThrottledPeriods"], object.Container, object.Release)
		result.Throttling.Periods = withCurrentRuntime(result.Throttling.Periods, verified["CPUPeriods"], object.Container, object.Release)
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
						samples, err = nativeSamples(values, metric)
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

func nativeSamples(rows []series, metric string) ([]sampledSeries, error) {
	out := make([]sampledSeries, 0, len(rows))
	for _, row := range rows {
		result := sampledSeries{labels: row.Metric, samples: make([]analysis.Sample, 0, len(row.Values))}
		for _, p := range row.Values {
			if math.IsNaN(p.Time) || math.IsInf(p.Time, 0) || p.Time <= 0 || p.Time >= float64(math.MaxInt64)/1000 || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 && !(metric == "CPUQuota" && p.Value == -1) {
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
	seen := make(map[string][]analysis.Sample)
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
		var samples []analysis.Sample
		for _, sample := range row.samples {
			if sample.Timestamp >= max(pod.CreatedAt, pod.ContainerStartedAt) {
				samples = append(samples, sample)
			}
		}
		if len(samples) > 0 {
			if previous, found := seen[pod.UID]; found {
				if !slices.Equal(samples, previous) {
					return nil
				}
				continue
			}
			seen[pod.UID] = samples
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

// Each CFS interval used as proof needs both endpoints at the limit under review.
// Mark the contiguous suffix at that quota. Classification uses the full series.
func atAppliedQuota(counters, quotas, periods []analysis.Series, limit float64) []analysis.Series {
	if len(counters) != len(quotas) || len(counters) != len(periods) {
		return nil
	}
	for i, counter := range counters {
		quota, period := quotas[i], periods[i]
		if counter.ID != quota.ID || counter.ID != period.ID {
			return nil
		}
		// cAdvisor timestamps CFS counters at collection and specification gauges
		// at scrape. Both quota gauges share a scrape timestamp with one another.
		q, p := len(quota.Samples)-1, len(period.Samples)-1
		for q >= 0 && p >= 0 && quota.Samples[q].Timestamp == period.Samples[p].Timestamp && period.Samples[p].Value > 0 &&
			math.Abs(quota.Samples[q].Value/period.Samples[p].Value-limit) <= 1e-9 {
			q--
			p--
		}
		atQuota := quota.Samples[q+1:]
		if len(atQuota) == 0 || len(counter.Samples) == 0 {
			return nil
		}
		first := len(counter.Samples) - 1
		latest := counter.Samples[first].Timestamp
		if latest < atQuota[0].Timestamp || latest > atQuota[len(atQuota)-1].Timestamp {
			return nil
		}
		// Each interval must lie inside the matching quota span and contain a
		// distinct confirming scrape. An observation gap ends the proof suffix.
		g := len(atQuota) - 1
		for first > 0 && counter.Samples[first-1].Timestamp >= atQuota[0].Timestamp {
			for g >= 0 && atQuota[g].Timestamp > counter.Samples[first].Timestamp {
				g--
			}
			if g < 0 || atQuota[g].Timestamp <= counter.Samples[first-1].Timestamp {
				break
			}
			first--
		}
		counters[i].CPUGuardStart = counter.Samples[first].Timestamp
	}
	return counters
}

func cgroupMatchesPod(id, uid string) bool {
	// cgroupfs and systemd spell the immutable Pod UID differently.
	return strings.Contains(id, "/pod"+uid+"/") ||
		strings.Contains(id, "-pod"+strings.ReplaceAll(uid, "-", "_")+".slice/")
}

func normalizeRuntimeID(id string) string {
	if _, suffix, ok := strings.Cut(id, "://"); ok {
		return suffix
	}
	id = strings.TrimRight(id, "/")
	id = id[strings.LastIndexByte(id, '/')+1:]
	leaf, scope := strings.CutSuffix(id, ".scope")
	for _, prefix := range []string{"cri-containerd-", "crio-", "docker-"} {
		if runtime, ok := strings.CutPrefix(leaf, prefix); ok {
			return runtime
		}
	}
	if scope {
		return ""
	}
	return id
}

func withCurrentRuntime(history, current []analysis.Series, container, release string) []analysis.Series {
	lifetime := func(series analysis.Series) string {
		return normalizeRuntimeID(strings.TrimPrefix(series.ID, series.PodUID+"/"+container+"/"))
	}
	active := make(map[string]string, len(current))
	for _, series := range current {
		active[series.PodUID] = lifetime(series)
	}
	for _, series := range history {
		if id, found := active[series.PodUID]; !found || series.Release != release || lifetime(series) != id {
			series.CPUGuardAdvisoryOnly = series.Release == release
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
			lifetime := row.labels["id"]
			if lifetime == "" {
				lifetime = row.labels["container_id"]
			}
			runtimeID := normalizeRuntimeID(lifetime)
			running := pod.ContainerStartedAt > 0 && runtimeID != "" && runtimeID == normalizeRuntimeID(pod.ContainerID)
			if row.labels["uid"] == "" && !running && !cgroupMatchesPod(lifetime, pod.UID) {
				continue
			}
			matched++
			lowerBound, upperBound := pod.CreatedAt, pod.EndedAt
			if running {
				lowerBound = max(lowerBound, pod.ContainerStartedAt)
			} else if lifetime != "" && pod.ContainerID != "" && pod.ContainerStartedAt > 0 {
				// A scrape of the preceding incarnation cannot attest to current usage.
				if upperBound == 0 || pod.ContainerStartedAt < upperBound {
					upperBound = pod.ContainerStartedAt
				}
			}
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
				if sample.Timestamp >= lowerBound && (upperBound == 0 || sample.Timestamp < upperBound) {
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

func convertOOMEvents(rows []sampledSeries, object model.Object, start, end int64) ([]analysis.OOMKill, bool) {
	incomplete := false
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
				if ts < max(start, pod.CreatedAt) || pod.EndedAt > 0 && ts >= pod.EndedAt {
					continue
				}
				if row.labels["uid"] == "" || ts > end {
					incomplete = incomplete || pod.Release == object.Release
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
	return kills, incomplete
}

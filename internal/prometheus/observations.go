package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
	result := strategy.Metrics{WindowStart: start.UnixMilli(), EvaluationTime: end.UnixMilli()}
	var warnings []string
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
		data, excluded := convertUsage(rows, object, kind)
		if excluded {
			warnings = append(warnings, metric+"UnattributedSeries")
		}
		if len(data) == 0 {
			warnings = append(warnings, "NoPrometheus"+metric)
		}
		if metric == "CPUUsage" {
			result.CPU = data
		} else {
			result.Memory = data
		}
	}
	return result, warnings
}

func (c *Client) loadMetric(ctx context.Context, metric string, object model.Object, start, end time.Time) ([]sampledSeries, error) {
	var combined []sampledSeries
	group, groupCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	for first := 0; first < len(object.Pods); first += 50 {
		last := min(first+50, len(object.Pods))
		batch := object
		batch.Pods = object.Pods[first:last]
		group.Go(func() error {
			query := BuildMetricQuery(metric, batch, c.cfg)
			// Keep each response bounded even for long lookbacks and many pods.
			chunkDuration := 6 * time.Hour
			for chunkStart := start; chunkStart.Before(end); {
				chunkEnd := chunkStart.Add(chunkDuration)
				if chunkEnd.After(end) {
					chunkEnd = end
				}
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
				combined = append(combined, samples...)
				mu.Unlock()
				chunkStart = chunkEnd
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	// A source series may occur in several time chunks. Merge only identical
	// label sets; keep duplicate source timestamps for the analyzer to validate.
	merged := make(map[string]sampledSeries)
	for _, row := range combined {
		key := labelsKey(row.labels)
		item := merged[key]
		item.labels = row.labels
		item.samples = append(item.samples, row.samples...)
		merged[key] = item
	}
	out := make([]sampledSeries, 0, len(merged))
	for _, row := range merged {
		out = append(out, row)
	}
	return out, nil
}

func nativeSamples(rows []series) ([]sampledSeries, error) {
	out := make([]sampledSeries, 0, len(rows))
	for _, row := range rows {
		result := sampledSeries{labels: row.Metric}
		for _, raw := range row.Values {
			p, err := parsePair(raw)
			if err != nil {
				return nil, err
			}
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
		result := sampledSeries{labels: row.Metric}
		for _, raw := range row.Values {
			p, err := parsePair(raw)
			if err != nil {
				return nil, err
			}
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
	excluded := false
	for _, row := range rows {
		candidates := pods[row.labels["pod"]]
		if len(candidates) == 0 {
			excluded = true
			continue
		}
		if ns := row.labels["namespace"]; ns != "" && ns != object.Namespace {
			excluded = true
			continue
		}
		matched := false
		for _, pod := range candidates {
			if pod.UID == "" || pod.Release == "" || pod.CreatedAt <= 0 || row.labels["uid"] != "" && row.labels["uid"] != pod.UID {
				continue
			}
			matched = true
			lifetime := row.labels["id"]
			if lifetime == "" {
				lifetime = row.labels["container_id"]
			}
			lowerBound := pod.CreatedAt
			if lifetime == "" {
				if pod.ContainerID == "" || pod.ContainerStartedAt <= 0 {
					excluded = true
					continue
				}
				lifetime, lowerBound = pod.ContainerID, max(lowerBound, pod.ContainerStartedAt)
			}
			id := pod.UID + "/" + object.Container + "/" + lifetime
			if seen[id] {
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
		if !matched {
			excluded = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, excluded
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

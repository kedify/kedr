package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

func pair(timestamp float64, value string) []json.RawMessage {
	return []json.RawMessage{json.RawMessage(strconv.FormatFloat(timestamp, 'f', -1, 64)), json.RawMessage(strconv.Quote(value))}
}

func TestUsageIdentityAndContainerLifetimes(t *testing.T) {
	object := model.Object{UID: "workload", Container: "app", Namespace: "ns", Pods: []model.Pod{{Name: "p", UID: "new-pod", Release: "A", CreatedAt: 10000, ContainerID: "runtime://new", ContainerStartedAt: 30000}}}
	rows := []sampledSeries{
		{labels: map[string]string{"pod": "p", "id": "old-container"}, samples: []analysis.Sample{{Timestamp: 9000, Value: 100}, {Timestamp: 12000, Value: 2}}},
		{labels: map[string]string{"pod": "p", "id": "new-container"}, samples: []analysis.Sample{{Timestamp: 32000, Value: 4}}},
		{labels: map[string]string{"pod": "p"}, samples: []analysis.Sample{{Timestamp: 12000, Value: 200}, {Timestamp: 33000, Value: 5}}},
		{labels: map[string]string{"pod": "p", "uid": "old-pod", "id": "old-uid"}, samples: []analysis.Sample{{Timestamp: 34000, Value: 300}}},
		{labels: map[string]string{"pod": "unknown", "id": "unowned"}, samples: []analysis.Sample{{Timestamp: 34000, Value: 400}}},
	}
	usage, excluded := convertUsage(rows, object, analysis.SampleCPUCounterSeconds)
	if !excluded || len(usage) != 3 {
		t.Fatalf("lifetimes/identity handling: %+v, excluded=%v", usage, excluded)
	}
	for _, s := range usage {
		if len(s.Samples) != 1 || s.PodUID != "new-pod" || s.WorkloadUID != "workload" || s.Release != "A" || s.Kind != analysis.SampleCPUCounterSeconds {
			t.Fatalf("invalid conversion: %+v", s)
		}
		if s.Samples[0].Value > 5 {
			t.Fatalf("old pod or missing-lifetime history attributed to current pod: %+v", s)
		}
	}
	object.Pods[0].ContainerID = ""
	if usage, _ = convertUsage(rows[:3], object, analysis.SampleGauge); len(usage) != 2 {
		t.Fatal("missing container lifetime was invented")
	}
}

func TestOOMUsesTerminationValueAndUnknownHistoricalLimit(t *testing.T) {
	object := model.Object{UID: "workload", Container: "app", Pods: []model.Pod{{Name: "p", UID: "pod-uid", Release: "A", CreatedAt: 1000}}}
	raw := []series{{Metric: map[string]string{"pod": "p", "uid": "pod-uid"}, Values: []samplePair{{100, 5}, {200, 5}}}}
	rows, err := eventSamples(raw)
	if err != nil {
		t.Fatal(err)
	}
	kills := convertOOMEvents(rows, object, 1000, 300000)
	if len(kills) != 1 || kills[0].Timestamp != 5000 || kills[0].MemoryLimitBytes != 0 || kills[0].ID != "pod-uid/app/5000" {
		t.Fatalf("bad OOM adapter: %+v", kills)
	}
	rows[0].labels["uid"] = "other-uid"
	if len(convertOOMEvents(rows, object, 1000, 300000)) != 0 {
		t.Fatal("other pod's OOM attributed by name")
	}
}

func TestRawQueriesNeverAggregateUsage(t *testing.T) {
	cfg := config.Default("simple_limit")
	cfg.UseOOMKillData = true
	object := model.Object{Namespace: "ns", Container: "app", Pods: []model.Pod{{Name: "app.1"}}}
	for _, name := range MetricsForStrategy(cfg) {
		q := BuildMetricQuery(name, object, cfg)
		for _, forbidden := range []string{"rate(", "quantile", "max_over_time", "count_over_time"} {
			if strings.Contains(q, forbidden) {
				t.Fatalf("usage pre-aggregated: %s", q)
			}
		}
		if name == "OOMKilledTimestamp" && (!strings.Contains(q, "last_terminated_timestamp") || !strings.Contains(q, "== 1") || strings.Contains(q, "resource_limits")) {
			t.Fatalf("unsafe OOM query: %s", q)
		}
	}
}

func TestMetricChunksPreserveAllHistory(t *testing.T) {
	cfg := config.Default("simple")
	client, err := New(context.Background(), cfg, "https://prometheus.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		query := r.URL.Query().Get("query")
		if r.URL.Path != "/api/v1/query" || strings.Contains(query, "timestamp(") {
			t.Errorf("must fetch native range vectors: %s", r.URL)
		}
		end, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		match := regexp.MustCompile(`\[(\d+)ms\]$`).FindStringSubmatch(query)
		if len(match) != 2 {
			t.Fatalf("missing range vector: %s", query)
		}
		duration, _ := strconv.ParseInt(match[1], 10, 64)
		if duration > (6 * time.Hour).Milliseconds() {
			t.Error("unbounded response window")
		}
		value := "10"
		body := fmt.Sprintf("{\"status\":\"success\",\"data\":{\"resultType\":\"matrix\",\"result\":[{\"metric\":{\"pod\":\"p\",\"id\":\"lifetime\"},\"values\":[[%g,%q]]}]}}", end, value)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	start := time.Unix(1800000000, 0)
	pods := make([]model.Pod, 50)
	for i := range pods {
		pods[i].Name = fmt.Sprintf("p-%d", i)
	}
	rows, err := client.loadMetric(context.Background(), "MemoryUsage", model.Object{Pods: pods}, start, start.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 8 || len(rows) != 1 || len(rows[0].samples) != 8 {
		t.Fatalf("chunk history lost: requests=%d, rows=%+v", requests.Load(), rows)
	}
	for i, sample := range rows[0].samples {
		if sample.Timestamp != start.Add(time.Duration(i+1)*6*time.Hour).UnixMilli() {
			t.Fatalf("chunks merged out of order: %+v", rows[0].samples)
		}
	}
}

func TestChunkLifetimeFilteringPreservesUsage(t *testing.T) {
	start := time.Unix(1800000000, 0)
	end := start.Add(8 * 300 * time.Hour)
	birth := start.Add(7 * 300 * time.Hour).UnixMilli()
	object := model.Object{UID: "workload", Namespace: "ns", Container: "app", Pods: []model.Pod{
		{Name: "p", UID: "old", Release: "A", CreatedAt: birth, EndedAt: birth + 1000},
		{Name: "p", UID: "new", Release: "B", CreatedAt: birth + 1000},
	}}
	cfg := config.Default("simple")
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "p|p") {
			t.Error("reused pod names should only be fetched once")
		}
		at, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		match := regexp.MustCompile(`\[(\d+)ms\]$`).FindStringSubmatch(query)
		duration, _ := strconv.ParseInt(match[1], 10, 64)
		points := []any{}
		for _, ts := range []int64{birth, birth + 1000, end.UnixMilli()} {
			if ts > int64(at*1000)-duration && ts <= int64(at*1000) {
				points = append(points, []any{float64(ts) / 1000, "10"})
			}
		}
		data, marshalErr := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"result": []any{map[string]any{"metric": map[string]string{"pod": "p", "id": "lifetime"}, "values": points}}}})
		if marshalErr != nil {
			return nil, marshalErr
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})}
	rows, err := client.loadMetric(context.Background(), "MemoryUsage", object, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("queried before pod creation: %d requests", requests.Load())
	}
	got, excluded := convertUsage(rows, object, analysis.SampleGauge)
	want, _ := convertUsage([]sampledSeries{{labels: map[string]string{"pod": "p", "id": "lifetime"}, samples: []analysis.Sample{
		{Timestamp: birth, Value: 10}, {Timestamp: birth + 1000, Value: 10}, {Timestamp: end.UnixMilli(), Value: 10},
	}}}, object, analysis.SampleGauge)
	if excluded || !reflect.DeepEqual(got, want) {
		t.Fatalf("lifetime boundary samples changed: got %+v, want %+v", got, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = client.loadMetric(ctx, "MemoryUsage", object, start, end); err == nil {
		t.Fatal("canceled collection returned success")
	}
}

func TestAdaptiveChunksBoundResponsesWithoutGaps(t *testing.T) {
	for _, tc := range []struct{ pods, requests int }{{1, 2}, {7, 8}, {50, 56}, {51, 112}} {
		t.Run(strconv.Itoa(tc.pods), func(t *testing.T) {
			cfg := config.Default("simple")
			client, err := New(context.Background(), cfg, "https://metrics.example", nil)
			if err != nil {
				t.Fatal(err)
			}
			start := time.UnixMilli(1800000000125)
			end := start.Add(14 * 24 * time.Hour)
			windows := map[string][][2]int64{}
			var mu sync.Mutex
			var requests atomic.Int32
			queryPattern := regexp.MustCompile(`pod=~("[^"]+").*\[(\d+)ms\]$`)
			client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				match := queryPattern.FindStringSubmatch(r.URL.Query().Get("query"))
				if len(match) != 3 {
					return nil, fmt.Errorf("unexpected query: %s", r.URL)
				}
				pattern, _ := strconv.Unquote(match[1])
				pods := strings.Split(pattern, "|")
				duration, _ := strconv.ParseInt(match[2], 10, 64)
				if duration*int64(len(pods)) > (300*time.Hour).Milliseconds() || len(pods) > 50 {
					t.Error("response exceeds pod/time budget")
				}
				at, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
				last := int64(at * 1000)
				mu.Lock()
				for _, pod := range pods {
					windows[pod] = append(windows[pod], [2]int64{last - duration, last})
				}
				mu.Unlock()
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"result":[]}}`))}, nil
			})}
			pods := make([]model.Pod, tc.pods)
			for i := range pods {
				pods[i].Name = fmt.Sprintf("p-%d", i)
			}
			if _, err = client.loadMetric(context.Background(), "CPUUsage", model.Object{Pods: pods}, start, end); err != nil {
				t.Fatal(err)
			}
			if int(requests.Load()) != tc.requests || len(windows) != tc.pods {
				t.Fatalf("requests=%d pods=%d, want %+v", requests.Load(), len(windows), tc)
			}
			for pod, chunks := range windows {
				sort.Slice(chunks, func(i, j int) bool { return chunks[i][0] < chunks[j][0] })
				cursor := start.UnixMilli()
				for _, chunk := range chunks {
					if chunk[0] != cursor {
						t.Fatalf("gap or overlap for %s: cursor=%d chunk=%v", pod, cursor, chunk)
					}
					cursor = chunk[1]
				}
				if cursor != end.UnixMilli() {
					t.Fatalf("truncated window for %s: %d", pod, cursor)
				}
			}
		})
	}
}

func TestOOMChunksRetainObservationsAfterPodEnded(t *testing.T) {
	cfg := config.Default("simple")
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1800000000, 0)
	termination := start.Add(30 * time.Minute)
	object := model.Object{UID: "workload", Container: "app", Pods: []model.Pod{{Name: "p", UID: "uid", Release: "A", CreatedAt: start.UnixMilli(), EndedAt: start.Add(time.Hour).UnixMilli()}}}
	var requests atomic.Int32
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.Path != "/api/v1/query_range" {
			t.Error("OOM expression must use query_range")
		}
		at, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		body := fmt.Sprintf(`{"status":"success","data":{"result":[{"metric":{"pod":"p"},"values":[[%d,"%d"]]}]}}`, at, termination.Unix())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	rows, err := client.loadMetric(context.Background(), "OOMKilledTimestamp", object, start, start.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	kills := convertOOMEvents(rows, object, start.UnixMilli(), start.Add(24*time.Hour).UnixMilli())
	if requests.Load() != 4 || len(kills) != 1 || kills[0].Timestamp != termination.UnixMilli() {
		t.Fatalf("OOM event lost: requests=%d kills=%+v", requests.Load(), kills)
	}
}

func TestNativeSamplesPreserveScrapesOffQueryGrid(t *testing.T) {
	rows := []series{{Metric: map[string]string{"pod": "p"}, Values: []samplePair{{60.125, 1}, {120.125, 2}, {180.125, 3}, {240.125, 4}}}}
	samples, err := nativeSamples(rows)
	if err != nil || len(samples[0].samples) != 4 || samples[0].samples[1].Timestamp != 120125 {
		t.Fatalf("native scrapes lost: %+v, %v", samples, err)
	}
}

func TestReusedStatefulPodNamesDoNotMergeReleases(t *testing.T) {
	object := model.Object{UID: "workload", Container: "app", Namespace: "ns", Pods: []model.Pod{
		{Name: "db-0", UID: "old", Release: "A", CreatedAt: 1000, EndedAt: 10000, Deleted: true},
		{Name: "db-0", UID: "new", Release: "B", CreatedAt: 10000},
	}}
	rows := []sampledSeries{{labels: map[string]string{"pod": "db-0", "id": "container"}, samples: []analysis.Sample{{Timestamp: 5000, Value: 100}, {Timestamp: 15000, Value: 10}}}}
	usage, excluded := convertUsage(rows, object, analysis.SampleGauge)
	if excluded || len(usage) != 2 {
		t.Fatalf("lost historical incarnation: %+v, %v", usage, excluded)
	}
	for _, row := range usage {
		if len(row.Samples) != 1 || row.Release == "A" && row.Samples[0].Value != 100 || row.Release == "B" && row.Samples[0].Value != 10 {
			t.Fatalf("pod incarnations mixed: %+v", row)
		}
	}
	kills := convertOOMEvents(rows, object, 1000, 20000)
	if len(kills) != 2 || kills[0].Release == kills[1].Release {
		t.Fatalf("OOM releases mixed: %+v", kills)
	}
}

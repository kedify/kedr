package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
	raw := []series{{Metric: map[string]string{"pod": "p", "uid": "pod-uid"}, Values: [][]json.RawMessage{pair(100, "5"), pair(200, "5")}}}
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
	var requests int
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
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
	rows, err := client.loadMetric(context.Background(), "MemoryUsage", model.Object{Pods: []model.Pod{{Name: "p"}}}, start, start.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if requests != 8 || len(rows) != 1 || len(rows[0].samples) != 8 {
		t.Fatalf("chunk history lost: requests=%d, rows=%+v", requests, rows)
	}
}

func TestNativeSamplesPreserveScrapesOffQueryGrid(t *testing.T) {
	rows := []series{{Metric: map[string]string{"pod": "p"}, Values: [][]json.RawMessage{pair(60.125, "1"), pair(120.125, "2"), pair(180.125, "3"), pair(240.125, "4")}}}
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

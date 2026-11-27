package prometheus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

const guardStart = 1700000000

const (
	labeledSeries   = `{"namespace":"demo","pod":"pod","container":"app","uid":"pod-uid","id":"/kubepods/pod-pod-uid/cri-containerd-abc.scope"}`
	unlabeledSeries = `{"namespace":"demo","pod":"pod","container":"app"}`
)

func guardObject() model.Object {
	return model.Object{Namespace: "demo", Kind: "Deployment", Name: "web", Container: "app", UID: "workload", Release: "release",
		Pods: []model.Pod{{Name: "pod", UID: "pod-uid", Release: "release", CreatedAt: guardStart * 1000, ContainerID: "containerd://abc", ContainerStartedAt: guardStart * 1000}}}
}

func matrixBody(rows ...string) string {
	return `{"status":"success","data":{"resultType":"matrix","result":[` + strings.Join(rows, ",") + `]}}`
}

func matrixRow(labels string, step, count int, value func(i int) float64) string {
	return matrixRowWithOffset(labels, step, count, 0, value)
}

func matrixRowWithOffset(labels string, step, count, offset int, value func(i int) float64) string {
	points := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		points = append(points, fmt.Sprintf(`[%d,"%g"]`, guardStart+i*step+offset, value(i)))
	}
	return fmt.Sprintf(`{"metric":%s,"values":[%s]}`, labels, strings.Join(points, ","))
}

func counter(i int) float64 { return float64(i) }

func respondAll(labels ...string) func(string) (int, string) {
	return func(string) (int, string) {
		rows := make([]string, 0, len(labels))
		for _, l := range labels {
			rows = append(rows, matrixRow(l, 60, 2, counter))
		}
		return 200, matrixBody(rows...)
	}
}

func fakePrometheus(t *testing.T, cfg *config.Config, respond func(query string) (int, string)) (*Client, func() []string) {
	t.Helper()
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var queries []string
	client.UseHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, query)
		mu.Unlock()
		status, body := respond(query)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
}

func TestSimpleLimitQueriesCFSIndependentOfClassifier(t *testing.T) {
	if got := MetricsForStrategy(config.Default("simple")); !slices.Equal(got, []string{"CPUUsage", "MemoryUsage"}) {
		t.Fatalf("simple query set changed: %v", got)
	}
	cfg := config.Default("simple_limit")
	want := []string{"CPUUsage", "MemoryUsage", "CPUThrottledSeconds", "CPUThrottledPeriods", "CPUPeriods", "CPUQuota", "CPUPeriod"}
	if got := MetricsForStrategy(cfg); !slices.Equal(got, want) {
		t.Fatalf("simple_limit without classifier: got %v, want %v", got, want)
	}
	cfg.ClassifyWorkloads = true
	if got := MetricsForStrategy(cfg); len(got) != 8 || !slices.Contains(got, "OOMKilledTimestamp") {
		t.Fatalf("classifier should add only OOM data: %v", got)
	}
	object := guardObject()
	for metric, prefix := range map[string]string{
		"CPUThrottledSeconds": "container_cpu_cfs_throttled_seconds_total{",
		"CPUThrottledPeriods": "container_cpu_cfs_throttled_periods_total{",
		"CPUPeriods":          "container_cpu_cfs_periods_total{",
	} {
		query := BuildMetricQuery(metric, object, cfg)
		if !strings.HasPrefix(query, prefix) || !strings.Contains(query, `namespace="demo",pod=~"pod",container="app"`) {
			t.Fatalf("%s query shape: %s", metric, query)
		}
	}
}

func TestSimpleGatherIssuesOnlyUsageQueries(t *testing.T) {
	client, queries := fakePrometheus(t, config.Default("simple"), respondAll(labeledSeries))
	metrics, _ := client.GatherWindow(context.Background(), guardObject(), time.Unix(guardStart, 0), time.Unix(guardStart+120, 0))
	got := queries()
	if len(got) != 2 {
		t.Fatalf("simple issued %d queries: %v", len(got), got)
	}
	for _, q := range got {
		if strings.Contains(q, "cfs") || strings.Contains(q, "kube_") {
			t.Fatalf("simple issued extra query %s", q)
		}
	}
	if metrics.Throttling.SourceComplete || len(metrics.Throttling.Seconds) != 0 {
		t.Fatalf("simple produced throttling evidence: %+v", metrics.Throttling)
	}
}

func TestCFSSourceCompleteIsConservative(t *testing.T) {
	verified := respondAll(labeledSeries)
	for _, tc := range []struct {
		name    string
		respond func(string) (int, string)
		want    bool
	}{
		{"verified runtime IDs", verified, true},
		{"pod status ID fallback", respondAll(unlabeledSeries), false},
		{"identical scrape mirrors", respondAll(
			`{"namespace":"demo","pod":"pod","container":"app","uid":"pod-uid","id":"/kubepods/pod-pod-uid/cri-containerd-abc.scope","job":"kubelet"}`,
			`{"namespace":"demo","pod":"pod","container":"app","uid":"pod-uid","id":"/kubepods/pod-pod-uid/cri-containerd-abc.scope","job":"cadvisor"}`), true},
		{"conflicting mirrors", func(q string) (int, string) {
			other := strings.TrimSuffix(labeledSeries, "}") + `,"job":"mirror"}`
			return 200, matrixBody(matrixRow(labeledSeries, 60, 2, counter), matrixRow(other, 60, 2, func(int) float64 { return 10 }))
		}, false},
		{"missing CFS series", func(q string) (int, string) {
			if strings.Contains(q, "cfs") {
				return 200, matrixBody()
			}
			return verified(q)
		}, false},
		{"failed CFS query", func(q string) (int, string) {
			if strings.HasPrefix(q, "container_cpu_cfs_periods_total") {
				return 422, `{"status":"error","errorType":"execution","error":"boom"}`
			}
			return verified(q)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := fakePrometheus(t, config.Default("simple_limit"), tc.respond)
			metrics, _ := client.GatherWindow(context.Background(), guardObject(), time.Unix(guardStart, 0), time.Unix(guardStart+120, 0))
			if metrics.Throttling.SourceComplete != tc.want {
				t.Fatalf("SourceComplete = %v, want %v (%+v)", metrics.Throttling.SourceComplete, tc.want, metrics.Throttling)
			}
		})
	}
}

func TestSimpleLimitWithoutCFSEvidenceKeepsCPULimit(t *testing.T) {
	cfg := config.Default("simple_limit")
	cfg.MinimumHistoryHours, cfg.PointsRequired = 1, 2
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	const points = 180
	client, _ := fakePrometheus(t, cfg, func(q string) (int, string) {
		switch {
		case strings.HasPrefix(q, "container_cpu_usage_seconds_total"):
			return 200, matrixBody(matrixRow(labeledSeries, 60, points, func(i int) float64 { return 0.6 * float64(i) }))
		case strings.HasPrefix(q, "container_memory_working_set_bytes"):
			return 200, matrixBody(matrixRow(labeledSeries, 60, points, func(int) float64 { return 100 << 20 }))
		}
		return 200, matrixBody() // Prometheus has no CFS series for this container.
	})
	end := time.Unix(guardStart+60*points, 0)
	object := guardObject()
	object.ObservedAt, object.InventoryAvailable = end.UnixMilli(), true
	requests := map[model.ResourceType]model.MaybeValue{model.CPU: model.Number(1), model.Memory: model.Number(512 << 20)}
	limits := map[model.ResourceType]model.MaybeValue{model.CPU: model.Number(2), model.Memory: model.Number(512 << 20)}
	object.Allocations.Requests, object.Allocations.Limits = requests, limits
	object.Pods[0].Allocations.Requests, object.Pods[0].Allocations.Limits = requests, limits
	metrics, _ := client.GatherWindow(context.Background(), object, time.Unix(guardStart, 0), end)
	if metrics.Throttling.SourceComplete {
		t.Fatal("missing CFS series certified complete")
	}
	if input := strategy.Input(cfg, metrics, object); input.Containers[0].Classification == nil {
		t.Fatal("CFS observations not passed to the shared engine with classifier disabled")
	}
	result, err := strategy.Run(cfg, metrics, object)
	if err != nil {
		t.Fatal(err)
	}
	if result.Analysis.EffectivePolicy.Classification != nil {
		t.Fatal("classification enabled implicitly")
	}
	if limit := result.Resources[model.CPU].Limit; limit.Set && limit.Value < 2 {
		t.Fatalf("CPU limit reduced without complete CFS evidence: %+v (%v)", limit, result.Resources[model.CPU].Info)
	}
	// The hold must come from the engine's strict guard, not from an unrelated
	// path that happens to keep the limit.
	if !slices.ContainsFunc(result.Analysis.Results, func(r analysis.ResourceAnalysis) bool {
		return r.Resource == analysis.ResourceCPU && slices.Contains(r.Notices, analysis.ReasonCPULimitEvidenceIncomplete)
	}) {
		t.Fatalf("engine did not hold a CPU limit for incomplete evidence: %+v", result.Analysis.Results)
	}
}

func TestCFSCertificationRequiresCurrentContainerRuntime(t *testing.T) {
	const oldSeries = `{"namespace":"demo","pod":"pod","container":"app","uid":"pod-uid","id":"/kubepods/pod-pod-uid/cri-containerd-before.scope"}`
	for _, tc := range []struct {
		name, usage, counters string
		withOld               bool
		want                  bool
	}{
		{name: "old and current", usage: labeledSeries, counters: labeledSeries, withOld: true, want: true},
		{name: "old counters only", usage: labeledSeries, counters: oldSeries},
		{name: "old CPU usage only", usage: oldSeries, counters: labeledSeries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := guardObject()
			object.Pods[0].ContainerStartedAt = (guardStart + 150) * 1000
			client, _ := fakePrometheus(t, config.Default("simple_limit"), func(query string) (int, string) {
				label := tc.usage
				if strings.Contains(query, "cfs") {
					label = tc.counters
				}
				values := []string{matrixRow(label, 60, 5, counter)}
				if tc.withOld {
					values = append(values, matrixRow(oldSeries, 60, 2, counter))
				}
				return 200, matrixBody(values...)
			})
			metrics, _ := client.GatherWindow(context.Background(), object, time.Unix(guardStart, 0), time.Unix(guardStart+300, 0))
			if metrics.Throttling.SourceComplete != tc.want {
				t.Fatalf("coverage = %v, want %v", metrics.Throttling.SourceComplete, tc.want)
			}
			if tc.want {
				rows := metrics.Throttling.Periods
				if len(rows) != 2 || len(rows[0].Samples) != 3 || rows[0].Samples[0].Timestamp < object.Pods[0].ContainerStartedAt ||
					rows[0].CPUGuardAdvisoryOnly || !rows[1].CPUGuardAdvisoryOnly || !strings.Contains(rows[1].ID, "before.scope") {
					t.Fatalf("expected certified current samples followed by advisory previous runtime: %+v", rows)
				}
			}
		})
	}
}

func TestSimpleLimitVerifiesRestartedContainer(t *testing.T) {
	const previous = "{\"namespace\":\"demo\",\"pod\":\"pod\",\"container\":\"app\",\"uid\":\"pod-uid\",\"id\":\"/kubepods/pod-pod-uid/cri-containerd-before.scope\"}"
	for _, tc := range []struct {
		name     string
		pressure bool
	}{
		{name: "clean restart allows reduction"},
		{name: "current pressure holds despite clean history", pressure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default("simple_limit")
			cfg.MinimumHistoryHours, cfg.PointsRequired = 1, 2
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			const points = 180
			client, _ := fakePrometheus(t, cfg, func(query string) (int, string) {
				oldValue, currentValue := func(int) float64 { return 0 }, func(int) float64 { return 0 }
				switch {
				case strings.HasPrefix(query, "container_spec_cpu_quota"):
					currentValue = func(int) float64 { return 200000 }
				case strings.HasPrefix(query, "container_spec_cpu_period"):
					currentValue = func(int) float64 { return 100000 }
				case strings.HasPrefix(query, "container_cpu_usage_seconds_total"):
					currentValue = func(i int) float64 { return .6 * float64(i) }
					oldValue = currentValue
				case strings.HasPrefix(query, "container_memory_working_set_bytes"):
					return 200, matrixBody(matrixRow(labeledSeries, 60, points, func(int) float64 { return 100 << 20 }))
				case strings.HasPrefix(query, "container_cpu_cfs_periods_total"):
					oldValue, currentValue = counter, counter
				case tc.pressure && strings.HasPrefix(query, "container_cpu_cfs_throttled_periods_total"):
					currentValue = counter
				}
				return 200, matrixBody(matrixRow(labeledSeries, 60, points, currentValue), matrixRow(previous, 60, points-4, oldValue))
			})
			end := time.Unix(guardStart+60*points, 0)
			object := guardObject()
			object.ObservedAt, object.InventoryAvailable = end.UnixMilli(), true
			object.Pods[0].ContainerStartedAt = end.Add(-3 * time.Minute).UnixMilli()
			object.Allocations.Requests = map[model.ResourceType]model.MaybeValue{model.CPU: model.Number(1), model.Memory: model.Number(512 << 20)}
			object.Allocations.Limits = map[model.ResourceType]model.MaybeValue{model.CPU: model.Number(2), model.Memory: model.Number(512 << 20)}
			object.Pods[0].Allocations = object.Allocations
			metrics, _ := client.GatherWindow(context.Background(), object, time.Unix(guardStart, 0), end)
			result, err := strategy.Run(cfg, metrics, object)
			if err != nil {
				t.Fatal(err)
			}
			limit := result.Resources[model.CPU].Limit
			if !metrics.Throttling.SourceComplete || result.Analysis.EffectivePolicy.Classification != nil ||
				(limit.Set && limit.Value < 2) == tc.pressure {
				t.Fatalf("unexpected guarded cap: limit=%+v expected pressure=%t analysis=%+v", limit, tc.pressure, result.Analysis.Results)
			}
			heldUnderPressure := slices.ContainsFunc(result.Analysis.Results, func(r analysis.ResourceAnalysis) bool {
				return r.Resource == analysis.ResourceCPU && slices.Contains(r.Notices, analysis.ReasonCPUThrottlingPressure)
			})
			if heldUnderPressure != tc.pressure {
				t.Fatalf("throttling outcome=%t want %t", heldUnderPressure, tc.pressure)
			}
		})
	}
}

func TestQuotaResizeRequiresIntervalsAtCurrentQuota(t *testing.T) {
	cfg := config.Default("simple_limit")
	cfg.ClassifyWorkloads = true
	cfg.MinimumHistoryHours, cfg.PointsRequired = 1, 2
	const points = 180
	for _, tc := range []struct {
		verified, offset int
		wantCut          bool
	}{{0, 0, false}, {1, 0, false}, {5, 0, true}, {1, -3, false}, {6, -3, true}} {
		client, _ := fakePrometheus(t, cfg, func(q string) (int, string) {
			value := func(int) float64 { return 0 }
			switch {
			case strings.HasPrefix(q, "container_cpu_usage_seconds_total"):
				value = func(i int) float64 { return .6 * float64(i) }
			case strings.HasPrefix(q, "container_memory_working_set_bytes"):
				value = func(int) float64 { return 100 << 20 }
			case strings.HasPrefix(q, "kube_pod_container_"):
				return 200, matrixBody()
			case strings.HasPrefix(q, "container_cpu_cfs_throttled_periods_total"):
				value = func(i int) float64 { return float64(min(i, points-5)) }
			case strings.HasPrefix(q, "container_cpu_cfs_periods_total"):
				value = counter
			case strings.HasPrefix(q, "container_spec_cpu_quota"):
				value = func(i int) float64 {
					if i == 1 {
						return -1
					} // The first scrape had no quota.
					if i <= points-tc.verified {
						return 400000
					}
					return 200000
				}
			case strings.HasPrefix(q, "container_spec_cpu_period"):
				value = func(int) float64 { return 100000 }
			}
			// Exact duplicate scrapes are equivalent to a single sample stream.
			other := strings.TrimSuffix(labeledSeries, "}") + `,"job":"mirror"}`
			offset := 0
			if strings.HasPrefix(q, "container_cpu_cfs_") {
				offset = tc.offset
			}
			return 200, matrixBody(matrixRowWithOffset(labeledSeries, 60, points, offset, value), matrixRowWithOffset(other, 60, points, offset, value))
		})
		end := time.Unix(guardStart+60*points, 0)
		obj := guardObject()
		obj.ObservedAt, obj.InventoryAvailable = end.UnixMilli(), true
		obj.Allocations = model.EmptyAllocations()
		obj.Allocations.Requests[model.CPU], obj.Allocations.Limits[model.CPU] = model.Number(1), model.Number(2)
		obj.Pods[0].Allocations = obj.Allocations
		metrics, _ := client.GatherWindow(context.Background(), obj, time.Unix(guardStart, 0), end)
		result, err := strategy.Run(cfg, metrics, obj)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(result.Analysis.Classifications[0].Features, analysis.FeatureCPUThrottled) {
			t.Fatal("pre-resize throttling disappeared from the release classification")
		}
		cpu := result.Resources[model.CPU]
		if cpu.Request.Value >= 1 || (cpu.Limit.Value < 2) != tc.wantCut {
			t.Fatalf("%+v produced %+v", tc, cpu)
		}
	}
}

func TestNewCapUsesCurrentUsageWithoutCFS(t *testing.T) {
	const points = 180
	for _, bursts := range []bool{false, true} {
		cfg := config.Default("simple_limit")
		cfg.MinimumHistoryHours, cfg.PointsRequired = 1, 2
		client, _ := fakePrometheus(t, cfg, func(q string) (int, string) {
			if strings.HasPrefix(q, "container_spec_cpu_") {
				t.Error("uncapped container unnecessarily queried for quota")
			}
			if !strings.HasPrefix(q, "container_cpu_usage_seconds_total") {
				return 200, matrixBody()
			}
			var total float64
			return 200, matrixBody(matrixRow(labeledSeries, 60, points, func(i int) float64 {
				rate := .020
				if bursts && (i == 10 || i == 30 || i == 50) {
					rate = .800
				}
				total += 60 * rate
				return total
			}))
		})
		end := time.Unix(guardStart+60*points, 0)
		obj := guardObject()
		obj.InventoryAvailable, obj.ObservedAt = true, end.UnixMilli()
		obj.Allocations = model.EmptyAllocations()
		obj.Allocations.Requests[model.CPU] = model.Number(.1)
		if bursts {
			obj.Allocations.Limits[model.CPU] = model.Number(0)
		}
		obj.Pods[0].Allocations = obj.Allocations
		metrics, _ := client.GatherWindow(context.Background(), obj, time.Unix(guardStart, 0), end)
		got, err := strategy.Run(cfg, metrics, obj)
		if err != nil {
			t.Fatal(err)
		}
		limit := got.Resources[model.CPU].Limit
		if (limit.Set && limit.Value > 0) == bursts {
			t.Fatalf("bursts=%t: %+v", bursts, got.Resources[model.CPU])
		}
	}
}

func TestCurrentCRIOCgroupfsSamples(t *testing.T) {
	obj := guardObject()
	obj.Pods[0].ContainerID = "cri-o://abc"
	labels := `{"namespace":"demo","pod":"pod","container":"app","id":"/kubepods/burstable/podpod-uid/crio-abc"}`
	client, _ := fakePrometheus(t, config.Default("simple"), respondAll(labels))
	metrics, _ := client.GatherWindow(context.Background(), obj, time.Unix(guardStart, 0), time.Unix(guardStart+120, 0))
	if len(metrics.CPU) != 1 || len(metrics.CPU[0].Samples) != 2 || len(metrics.Memory) != 1 || len(metrics.Memory[0].Samples) != 2 {
		t.Fatalf("running CRI-O cgroupfs samples disappeared: %+v", metrics)
	}
}

func TestAppliedQuotaBracketsNativeCounterTimes(t *testing.T) {
	series := func(times []int64, value float64) []analysis.Series {
		s := analysis.Series{ID: "runtime"}
		for _, ts := range times {
			s.Samples = append(s.Samples, analysis.Sample{Timestamp: ts, Value: value})
		}
		return []analysis.Series{s}
	}
	cfs := []int64{60, 120, 180, 240, 300, 360}
	for _, tc := range []struct {
		name          string
		quota, period []int64
		start         int64
	}{
		{"native collection precedes scrape", []int64{63, 123, 183, 243, 303, 363}, []int64{63, 123, 183, 243, 303, 363}, 120},
		{"missing spec scrape cannot bridge interval", []int64{63, 123, 303, 363}, []int64{63, 123, 303, 363}, 300},
		{"quota omitted while period remains", []int64{63, 123, 303, 363}, []int64{63, 123, 183, 243, 303, 363}, 360},
		{"missing latest scrape", []int64{63, 123, 183, 243, 303}, []int64{63, 123, 183, 243, 303}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := atAppliedQuota(series(cfs, 0), series(tc.quota, 200000), series(tc.period, 100000), 2)
			if tc.start == 0 {
				if len(got) != 0 {
					t.Fatal("unbracketed latest CFS accepted")
				}
				return
			}
			if len(got) != 1 || got[0].CPUGuardStart != tc.start || len(got[0].Samples) != len(cfs) {
				t.Fatalf("want start %d without lost classification; got %+v", tc.start, got)
			}
		})
	}
}

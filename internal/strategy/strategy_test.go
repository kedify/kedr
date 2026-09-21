package strategy

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

const testEpoch int64 = 1800000000000
const testMiB = 1024 * 1024

func fixture(hours int) (*config.Config, Metrics, model.Object) {
	cfg := config.Default("simple")
	cfg.MinimumHistoryHours = 3
	metrics := Metrics{WindowStart: testEpoch, EvaluationTime: testEpoch + int64(hours)*3600000}
	allocations := model.EmptyAllocations()
	allocations.Requests[model.CPU], allocations.Limits[model.CPU] = model.Number(1), model.Number(2)
	allocations.Requests[model.Memory], allocations.Limits[model.Memory] = model.Number(512*testMiB), model.Number(1024*testMiB)
	object := model.Object{Name: "app", Namespace: "ns", Kind: "Deployment", Container: "main", UID: "workload", Release: "release-a", ObservedAt: metrics.EvaluationTime, ReleaseStartedAt: testEpoch, InventoryAvailable: true, Allocations: allocations}
	object.Pods = []model.Pod{{Name: "p", UID: "pod-uid", Release: object.Release, CreatedAt: testEpoch, Allocations: allocations}}
	cpu := analysis.Series{ID: "cpu-lifetime", PodUID: "pod-uid", WorkloadUID: object.UID, Release: object.Release, Kind: analysis.SampleCPUCounterSeconds}
	memory := analysis.Series{ID: "memory-lifetime", PodUID: "pod-uid", WorkloadUID: object.UID, Release: object.Release, Kind: analysis.SampleGauge}
	for minute := 0; minute <= hours*60; minute++ {
		ts := testEpoch + int64(minute)*60000
		cpu.Samples = append(cpu.Samples, analysis.Sample{Timestamp: ts, Value: float64(minute) * 12})
		memory.Samples = append(memory.Samples, analysis.Sample{Timestamp: ts, Value: 128 * testMiB})
	}
	metrics.CPU, metrics.Memory = []analysis.Series{cpu}, []analysis.Series{memory}
	return cfg, metrics, object
}

func run(t *testing.T, cfg *config.Config, metrics Metrics, object model.Object) Result {
	t.Helper()
	result, err := Run(cfg, metrics, object)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSharedAnalyzerParity(t *testing.T) {
	for _, name := range []string{"simple", "simple_limit"} {
		cfg, metrics, object := fixture(4)
		cfg.Strategy = name
		actual := run(t, cfg, metrics, object)
		policy, err := cfg.AnalysisPolicy()
		if err != nil {
			t.Fatal(err)
		}
		expected, err := analysis.Analyze(Input(cfg, metrics, object), policy)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*actual.Analysis, expected) {
			t.Fatal("kedr did not return the shared analyzer output")
		}
		if actual.Resources[model.CPU].Request.Value != .2 {
			t.Fatalf("CPU counter conversion: %+v", actual.Resources[model.CPU])
		}
		wantLimit := 2.0
		if name == "simple_limit" {
			wantLimit = 1
		}
		if actual.Resources[model.CPU].Limit.Value != wantLimit {
			t.Fatalf("CPU limit=%+v", actual.Resources[model.CPU])
		}
		if actual.Resources[model.Memory].Request.Value != 128*testMiB*1.15 {
			t.Fatalf("memory=%+v", actual.Resources[model.Memory])
		}
		if actual.Analysis.Results[0].MemoryLeak != nil {
			t.Fatal("leak detection enabled by default")
		}
	}
}

func TestNearestRankPerSeriesNotPooledOrInterpolated(t *testing.T) {
	cfg, metrics, object := fixture(4)
	cfg.Strategy, cfg.CPURequest, cfg.CPULimitRatio = "simple_limit", 66, 2
	cumulative := 0.0
	for i := range metrics.CPU[0].Samples {
		if i > 0 {
			cumulative += float64((i-1)%4+1) * 6
		}
		metrics.CPU[0].Samples[i].Value = cumulative
	}
	other := metrics.CPU[0]
	other.ID, other.PodUID = "quiet-lifetime", "other-pod"
	other.Samples = append([]analysis.Sample(nil), other.Samples...)
	for i := range other.Samples {
		other.Samples[i].Value = float64(i) * .06
	}
	metrics.CPU = append(metrics.CPU, other)
	object.Pods = append(object.Pods, model.Pod{Name: "other", UID: "other-pod", Release: object.Release, Allocations: object.Allocations})
	actual := run(t, cfg, metrics, object).Resources[model.CPU]
	if actual.Request.Value != .3 {
		t.Fatalf("want nearest-rank P66=300m from busiest series, got %+v", actual)
	}
	cfg.CPURequest = 100
	if actual = run(t, cfg, metrics, object).Resources[model.CPU]; actual.Request.Value != .4 {
		t.Fatalf("P100 must map to max: %+v", actual)
	}
}

func TestSharedGuardsAndNoActionMapping(t *testing.T) {
	cfg, metrics, object := fixture(4)
	cfg.MinimumHistoryHours = 168
	result := run(t, cfg, metrics, object)
	if !result.Resources[model.CPU].Request.Unknown || !strings.Contains(*result.Resources[model.CPU].Info, "insufficient-history") {
		t.Fatal("short history bypassed shared guard")
	}
	cfg.MinimumHistoryHours = 3
	object.Allocations.Requests[model.CPU] = model.Number(.20123)
	object.Pods[0].Allocations = object.Allocations
	result = run(t, cfg, metrics, object)
	if got := result.Resources[model.CPU].Request; got.Unknown || got.Value != .20123 {
		t.Fatalf("no-action must retain exact current request, got %+v", got)
	}
	object.InventoryAvailable = false
	object.Allocations.Requests[model.Memory] = model.Number(512 * testMiB)
	result = run(t, cfg, metrics, object)
	if got := result.Resources[model.Memory].Request; got.Value != 512*testMiB {
		t.Fatalf("unknown inventory permitted downsizing: %+v", got)
	}
	object.IdentityAmbiguous = true
	if result = run(t, cfg, metrics, object); !result.Resources[model.Memory].Request.Unknown {
		t.Fatal("ambiguous identity permitted sizing")
	}
}

func TestInputUnitsSettingsAndOOMOptIn(t *testing.T) {
	cfg, metrics, object := fixture(4)
	kill := analysis.OOMKill{ID: "kill", PodUID: "pod-uid", WorkloadUID: object.UID, Release: object.Release, Timestamp: metrics.EvaluationTime, MemoryLimitBytes: 512 * testMiB}
	object.OOMKills = []analysis.OOMKill{kill}
	in := Input(cfg, metrics, object)
	if in.Containers[0].CPU.CurrentRequest.Value != 1000 || in.Containers[0].Memory.CurrentRequest.Value != 512*testMiB || len(in.Containers[0].OOMKills) != 0 {
		t.Fatalf("wrong input conversion: %+v", in.Containers[0])
	}
	cfg.UseOOMKillData = true
	result := run(t, cfg, metrics, object)
	if result.Resources[model.Memory].Request.Value != 640*testMiB || !strings.Contains(*result.Resources[model.Memory].Info, "oom-kill-detected") {
		t.Fatalf("OOM did not reach analyzer/report: %+v", result)
	}
	// Unknown termination-time limit must use the engine's conservative fallback.
	object.OOMKills[0].MemoryLimitBytes = 0
	if result = run(t, cfg, metrics, object); result.Resources[model.Memory].Request.Value != 512*testMiB {
		t.Fatal("unknown failed limit must block downsizing")
	}
	// Observed absence is zero; inconsistent settings across replicas are missing.
	object.Allocations.Limits[model.CPU] = model.Unset()
	object.Pods[0].Allocations = object.Allocations
	in = Input(cfg, metrics, object)
	if !in.Containers[0].CPU.CurrentLimit.Available || in.Containers[0].CPU.CurrentLimit.Value != 0 {
		t.Fatal("unset limit lost observed-zero semantics")
	}
	different := model.EmptyAllocations()
	object.Pods[0].Allocations = different
	if in = Input(cfg, metrics, object); in.Containers[0].CPU.CurrentRequest.Available {
		t.Fatal("inconsistent current settings were not marked unavailable")
	}
}

func TestMemoryLeakOptInAndHPASuppression(t *testing.T) {
	cfg, metrics, object := fixture(24)
	cfg.DetectMemoryLeaks = true
	for i := range metrics.Memory[0].Samples {
		metrics.Memory[0].Samples[i].Value = (100 + float64(i)/60*12) * testMiB
	}
	cpu := 50.0
	object.HPA = &model.HPA{TargetCPUPercent: &cpu}
	result := run(t, cfg, metrics, object)
	if !result.Resources[model.CPU].Request.Unknown || result.SuppressedResources[model.CPU] == "" {
		t.Fatal("HPA guard lost")
	}
	if result.Analysis.Results[0].MemoryLeak.Status != analysis.MemoryLeakPotential || !strings.Contains(*result.Resources[model.Memory].Info, "potential-memory-leak") {
		t.Fatal("leak finding missing from report")
	}
	if _, err := json.Marshal(result.Analysis); err != nil {
		t.Fatal(err)
	}
	cfg.AllowHPA = true
	if result = run(t, cfg, metrics, object); result.Resources[model.CPU].Request.Unknown || len(result.SuppressedResources) != 0 {
		t.Fatal("allow-hpa not respected")
	}
}

func TestInvalidSamplesReturnError(t *testing.T) {
	cfg, metrics, object := fixture(4)
	metrics.Memory[0].Samples[0].Value = math.NaN()
	if _, err := Run(cfg, metrics, object); err == nil {
		t.Fatal("invalid analyzer input was hidden")
	}
}

func TestPreviousReleaseIsComparisonOnlyIncludingOOMs(t *testing.T) {
	cfg, metrics, object := fixture(8)
	cfg.UseOOMKillData = true
	object.Releases = []model.Release{{ID: object.Release, Name: "rs-fixed", Image: "app:fixed", Current: true}, {ID: "old", Name: "rs-old", Image: "app:leaky"}}
	baseline := run(t, cfg, metrics, object)
	oldCPU, oldMemory := metrics.CPU[0], metrics.Memory[0]
	oldCPU.ID, oldCPU.PodUID, oldCPU.Release = "old-cpu", "old-pod", "old"
	oldMemory.ID, oldMemory.PodUID, oldMemory.Release = "old-memory", "old-pod", "old"
	oldCPU.Samples = append([]analysis.Sample(nil), oldCPU.Samples...)
	oldMemory.Samples = append([]analysis.Sample(nil), oldMemory.Samples...)
	for i := range oldCPU.Samples {
		oldCPU.Samples[i].Value *= 10
		oldMemory.Samples[i].Value *= 10
	}
	// The old release overlaps the current one through the evaluation time.
	// Its presence must not truncate current history or change its sizing.
	metrics.CPU, metrics.Memory = append(metrics.CPU, oldCPU), append(metrics.Memory, oldMemory)
	metrics.OOMKills = []analysis.OOMKill{{ID: "old-oom", PodUID: "old-pod", WorkloadUID: object.UID, Release: "old", Timestamp: metrics.EvaluationTime - 60000, MemoryLimitBytes: 4096 * testMiB}}
	got := run(t, cfg, metrics, object)
	if !reflect.DeepEqual(got.Analysis, baseline.Analysis) || !reflect.DeepEqual(got.Resources, baseline.Resources) {
		t.Fatal("old image or OOM affected newest-release analysis")
	}
	if len(got.ReleaseComparisons) != 2 || got.ReleaseComparisons[1].CPU.AggregatedUsage.Value != 2000 || got.ReleaseComparisons[1].Memory.AggregatedUsage.Value != 1280*testMiB || got.ReleaseComparisons[1].Memory.OOMKills != 1 {
		t.Fatalf("comparison evidence lost: %+v", got.ReleaseComparisons)
	}
	if got.ReleaseComparisons[0].Memory.OOMKills != 0 {
		t.Fatal("historical OOM included in current release")
	}
}

func TestDeletedPodsExtendCurrentReleaseHistory(t *testing.T) {
	cfg, metrics, object := fixture(8 * 24)
	cfg.MinimumHistoryHours = 168
	object.ReleaseStartedAt = 0
	object.Pods[0].CreatedAt = metrics.EvaluationTime - 4*3600000
	oldCPU, oldMemory := metrics.CPU[0], metrics.Memory[0]
	oldCPU.ID, oldCPU.PodUID = "deleted-cpu", "deleted-pod"
	oldMemory.ID, oldMemory.PodUID = "deleted-memory", "deleted-pod"
	cut := len(metrics.CPU[0].Samples) - 4*60
	oldCPU.Samples, oldMemory.Samples = oldCPU.Samples[:cut], oldMemory.Samples[:cut]
	metrics.CPU[0].Samples, metrics.Memory[0].Samples = metrics.CPU[0].Samples[cut:], metrics.Memory[0].Samples[cut:]
	short := run(t, cfg, metrics, object)
	if !short.Resources[model.Memory].Request.Unknown {
		t.Fatal("four hours unexpectedly satisfied seven-day minimum")
	}
	metrics.CPU, metrics.Memory = append(metrics.CPU, oldCPU), append(metrics.Memory, oldMemory)
	object.Pods = append(object.Pods, model.Pod{Name: "deleted", UID: "deleted-pod", Release: object.Release, CreatedAt: testEpoch, Deleted: true})
	got := run(t, cfg, metrics, object)
	if got.Resources[model.Memory].Request.Unknown || got.Resources[model.CPU].Request.Unknown || got.Analysis.Results[0].DataQuality.ObservedIntervalHours < 191 {
		t.Fatalf("deleted pod history did not reach analyzer: %+v", got)
	}
	if got.Analysis.Results[0].Evidence.Inventory.Eligible != 1 {
		t.Fatal("deleted pods were counted as live inventory")
	}
}

func TestNewReleaseDoesNotBorrowOldHistoryToPassMinimum(t *testing.T) {
	cfg, metrics, object := fixture(4)
	cfg.MinimumHistoryHours = 168
	object.Releases = []model.Release{{ID: object.Release, Current: true}, {ID: "old"}}
	old := metrics.Memory[0]
	old.ID, old.Release = "old", "old"
	old.Samples = append([]analysis.Sample(nil), old.Samples...)
	for i := range old.Samples {
		old.Samples[i].Timestamp -= 8 * 24 * 3600000
	}
	metrics.WindowStart -= 8 * 24 * 3600000
	metrics.Memory = append(metrics.Memory, old)
	got := run(t, cfg, metrics, object)
	if !got.Resources[model.Memory].Request.Unknown || !strings.Contains(*got.Resources[model.Memory].Info, "required 168.0h") {
		t.Fatal("older release bypassed the new release's evidence requirement")
	}
}

func TestCollectionOutageDoesNotHideRecommendations(t *testing.T) {
	cfg, metrics, object := fixture(8 * 24)
	cfg.MinimumHistoryHours = 168
	for _, rows := range [][]analysis.Series{metrics.CPU, metrics.Memory} {
		for i := range rows {
			// A collector restart loses fifteen minutes midway through eight days.
			rows[i].Samples = append(rows[i].Samples[:4*24*60], rows[i].Samples[4*24*60+15:]...)
		}
	}
	got := run(t, cfg, metrics, object)
	if got.Resources[model.CPU].Request.Unknown || got.Resources[model.Memory].Request.Unknown {
		t.Fatalf("historical collection outage hid recommendations: %+v", got)
	}
	if got.Analysis.SchemaVersion != analysis.OutputSchemaVersion || got.Analysis.DetectorVersion != analysis.ResourceRightSizeDetectorVersion {
		t.Fatal("kedr did not use the updated shared analyzer")
	}
	for _, result := range got.Analysis.Results {
		if result.DataQuality.Coverage < .99 || result.DataQuality.Coverage >= 1 {
			t.Fatalf("missing measurements should still reduce coverage: %+v", result.DataQuality)
		}
	}
}

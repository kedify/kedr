package explain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/recommend"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

func fixture(t *testing.T) (runstore.Run, runstore.Row, strategy.Metrics) {
	t.Helper()
	end := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC).UnixMilli()
	start := end - 6*3600000
	a := model.EmptyAllocations()
	a.Requests[model.CPU] = model.Number(.5)
	a.Limits[model.CPU] = model.Number(2)
	a.Requests[model.Memory] = model.Number(512 * 1024 * 1024)
	a.Limits[model.Memory] = model.Number(1024 * 1024 * 1024)
	object := model.Object{Name: "checkout", Namespace: "production", Kind: "Deployment", Container: "app", UID: "workload", Release: "v2", ObservedAt: end, ReleaseStartedAt: start, InventoryAvailable: true, Allocations: a, Pods: []model.Pod{{Name: "checkout-abc", UID: "pod", Release: "v2", CreatedAt: start, Allocations: a}}, Releases: []model.Release{{ID: "v2", Name: "checkout-7f8c9", Image: "checkout:2.4.0", Revision: 12, CreatedAt: start, Current: true}, {ID: "v1", Name: "checkout-6a7b8", Image: "checkout:2.3.0", Revision: 11, CreatedAt: start - 3600000}}}
	cpu := analysis.Series{ID: "pod/app/lifetime", PodUID: "pod", WorkloadUID: "workload", Release: "v2", Kind: analysis.SampleCPUCounterSeconds}
	mem := cpu
	mem.Kind = analysis.SampleGauge
	for i := 0; i <= 360; i++ {
		cpu.Samples = append(cpu.Samples, analysis.Sample{Timestamp: start + int64(i)*60000, Value: float64(i) * 12})
		v := 220.0 + float64(i%24)*3
		if i == 219 {
			v = 320
		}
		mem.Samples = append(mem.Samples, analysis.Sample{Timestamp: start + int64(i)*60000, Value: v * 1024 * 1024})
	}
	oldCPU := analysis.Series{ID: "old-pod/app/lifetime", PodUID: "old-pod", WorkloadUID: "workload", Release: "v1", Kind: analysis.SampleCPUCounterSeconds}
	oldMemory := oldCPU
	oldMemory.Kind = analysis.SampleGauge
	for i := 0; i < 180; i++ {
		at := start - 3*3600000 + int64(i)*60000
		oldCPU.Samples = append(oldCPU.Samples, analysis.Sample{Timestamp: at, Value: float64(i) * 18})
		oldMemory.Samples = append(oldMemory.Samples, analysis.Sample{Timestamp: at, Value: (300 + float64(i%20)*2) * 1024 * 1024})
	}
	object.Pods = append(object.Pods, model.Pod{Name: "checkout-old", UID: "old-pod", Release: "v1", CreatedAt: start - 3*3600000, EndedAt: start, Deleted: true})
	object.Releases[1].CreatedAt = start - 3*3600000
	metrics := strategy.Metrics{WindowStart: start - 3*3600000, EvaluationTime: end, CPU: []analysis.Series{cpu, oldCPU}, Memory: []analysis.Series{mem, oldMemory}, OOMKills: []analysis.OOMKill{{ID: "oom", PodUID: "pod", WorkloadUID: "workload", Release: "v2", Timestamp: start + 2*3600000, MemoryLimitBytes: 256 * 1024 * 1024}}}
	cfg := config.Default("simple")
	cfg.UseOOMKillData = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := strategy.Run(cfg, metrics, object)
	if err != nil {
		t.Fatal(err)
	}
	row := runstore.NewRow(recommend.Scan(object, raw), metrics, runstore.Connection{})
	row.Number = 1
	return runstore.Run{ID: "fixture", CreatedAt: time.UnixMilli(end), Strategy: "simple", KedrVersion: "test", Rows: []runstore.Row{row}}, row, metrics
}
func TestDecisionAndVerification(t *testing.T) {
	run, row, metrics := fixture(t)
	d := Build(run, row)
	d.AddMetrics(row, metrics, nil)
	if len(d.Cards) != 4 || !strings.Contains(d.ChartStatus, "Verified") {
		t.Fatalf("document: %+v", d)
	}
	if d.Cards[1].Outcome != "disabled" || !strings.Contains(d.Cards[1].Reason, "preserved") {
		t.Fatal("simple CPU limit must be explained as preserved")
	}
	if d.Cards[2].Recommended != "368Mi" {
		t.Fatalf("memory recommendation: %s", d.Cards[2].Recommended)
	}
	metrics.Memory[0].Samples[219].Value *= 2
	d.AddMetrics(row, metrics, nil)
	if !strings.Contains(d.ChartStatus, "Changed") || d.Cards[2].Recommended != "368Mi" {
		t.Fatal("fresh metrics replaced saved decision")
	}
	row.Scan.SuppressedResources = map[model.ResourceType]string{model.CPU: "HPA detected"}
	d = Build(run, row)
	if d.Cards[0].Outcome != "suppressed" || d.Cards[1].Outcome != "suppressed" {
		t.Fatal("HPA suppression not final layer")
	}
}
func TestPeakReductionAndGaps(t *testing.T) {
	var points []analysis.Sample
	for i := 0; i < 10000; i++ {
		v := 10.0
		if i == 4521 {
			v = 999
		}
		points = append(points, analysis.Sample{Timestamp: int64(i) * 1000, Value: v})
	}
	reduced := reduce(points, 20)
	found := false
	for _, p := range reduced {
		if p.Value == 999 {
			found = true
		}
	}
	if !found || len(reduced) > 80 || reduced[0] != points[0] || reduced[len(reduced)-1] != points[len(points)-1] {
		t.Fatal("reduction lost extrema/endpoints")
	}
	points[7000].Timestamp += 10000000
	// Gap splitting is applied to normalized, sorted observations.
	gap := []analysis.Sample{{Timestamp: 1000}, {Timestamp: 2000}, {Timestamp: 100000}, {Timestamp: 101000}}
	if got := segments(gap, 1000); len(got) != 2 {
		t.Fatalf("gap bridged: %v", got)
	}
}
func TestUnsetInitializationExplanation(t *testing.T) {
	run, row, metrics := fixture(t)
	object := row.Scan.Object
	object.Allocations.Requests[model.CPU] = model.Unset()
	object.Pods[0].Allocations.Requests[model.CPU] = model.Unset()
	for i := range metrics.CPU[0].Samples {
		metrics.CPU[0].Samples[i].Value = float64(i) * 60 * .024
	}
	raw, err := strategy.Run(config.Default("simple"), metrics, object)
	if err != nil {
		t.Fatal(err)
	}
	row.Scan = recommend.Scan(object, raw)
	// Saved JSON must retain the unset marker for later offline explanations.
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var saved runstore.Row
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	card := Build(run, saved).Cards[0]
	if card.Current != "unset" || card.Recommended != "24m" || card.Outcome != "recommended" || !strings.Contains(card.Reason, "minimum-change thresholds do not apply") {
		t.Fatalf("initialization was not explained: %+v", card)
	}
	for _, step := range card.Steps {
		if step.Rule == "material change" {
			t.Fatal("initialization explanation still compares against zero")
		}
	}
}

func TestHTMLSafeAndStandalone(t *testing.T) {
	run, row, metrics := fixture(t)
	attack := "</script><script>alert('unsafe')</script>"
	row.Scan.Object.Name = attack
	row.Identity.Pods[0].Name = attack
	d := Build(run, row)
	d.AddMetrics(row, metrics, nil)
	output, err := HTML(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(output)
	if strings.Contains(s, attack) || strings.Contains(s, "src=\"http") || strings.Contains(s, "ZgotmplZ") {
		t.Fatal("unsafe or broken template")
	}
	if !strings.Contains(s, "\\u003c/script\\u003e") || !strings.Contains(s, "&lt;/script&gt;") {
		t.Fatal("expected safe JSON and HTML escaping")
	}
	if !strings.Contains(s, "container_memory_working_set_bytes") {
		t.Fatal("queries missing")
	}
}

// Opt-in artifacts support visual inspection without committing generated output.
func TestVisualFixtures(t *testing.T) {
	dir := os.Getenv("KEDR_VISUAL_DIR")
	if dir == "" {
		t.Skip("set KEDR_VISUAL_DIR to export review fixtures")
	}
	run, row, metrics := fixture(t)
	d := Build(run, row)
	d.AddMetrics(row, metrics, nil)
	for _, name := range []string{"rich", "offline"} {
		if name == "offline" {
			d.Charts = nil
			d.ChartStatus = "Charts unavailable: historical data has expired."
		}
		data, err := HTML(d)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name+".html"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

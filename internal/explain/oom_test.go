package explain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/recommend"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

func oomFixture(t *testing.T) (runstore.Run, runstore.Row, strategy.Metrics) {
	t.Helper()
	run, row, metrics, _ := oomFixtureResult(t, false)
	return run, row, metrics
}

// oomFixtureResult assembles an OOM-only strategy fixture. The current-rollout
// event carries an explicit synthetic 50 MiB termination-time limit only when
// anchored; otherwise its limit is unknown, as KEDR's live sources report it.
func oomFixtureResult(t *testing.T, anchored bool) (runstore.Run, runstore.Row, strategy.Metrics, strategy.Result) {
	t.Helper()
	run, row, metrics := fixture(t)
	object := row.Scan.Object
	object.Allocations.Requests[model.CPU], object.Allocations.Limits[model.CPU] = model.Unset(), model.Unset()
	object.Allocations.Requests[model.Memory], object.Allocations.Limits[model.Memory] = model.Number(50*1024*1024), model.Number(50*1024*1024)
	for i := range object.Pods {
		object.Pods[i].Deleted = true
	}
	object.OOMKills = []analysis.OOMKill{
		{ID: "current-oom", PodUID: "pod", WorkloadUID: object.UID, Release: object.Release, Timestamp: metrics.EvaluationTime - 300000},
		{ID: "old-oom", PodUID: "old-pod", WorkloadUID: object.UID, Release: "v1", Timestamp: metrics.WindowStart + 60000},
	}
	if anchored {
		// Explicit synthetic event-time limit, used only for positive OOM sizing.
		object.OOMKills[0].MemoryLimitBytes = 50 * 1024 * 1024
	}
	metrics.CPU, metrics.Memory, metrics.OOMKills = nil, nil, nil
	cfg := config.Default("simple")
	cfg.UseOOMKillData = true
	result, err := strategy.Run(cfg, metrics, object)
	if err != nil {
		t.Fatal(err)
	}
	row = runstore.NewRow(recommend.Scan(object, result), metrics, runstore.Connection{OOM: true})
	row.Number = 1
	// Exercise the persisted representation used by kedr explain.
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &row); err != nil {
		t.Fatal(err)
	}
	run.Rows = []runstore.Row{row}
	return run, row, metrics, result
}

func TestOOMExplanationWithoutUsage(t *testing.T) {
	run, row, metrics, _ := oomFixtureResult(t, true)
	before, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	d := Build(run, row)
	for _, stage := range []string{"offline", "empty fetch", "changed fetch"} {
		switch stage {
		case "empty fetch":
			d.AddMetrics(row, metrics, []string{"NoPrometheusMemoryUsage"})
			if !strings.Contains(d.ChartStatus, "No historical usage samples") {
				t.Fatal(d.ChartStatus)
			}
		case "changed fetch":
			metrics.Memory = []analysis.Series{{ID: "later-data", PodUID: "pod", WorkloadUID: row.Identity.UID, Release: row.Identity.Release, Kind: analysis.SampleGauge, Samples: []analysis.Sample{{Timestamp: metrics.EvaluationTime, Value: 999 * 1024 * 1024}}}}
			metrics.OOMKills = []analysis.OOMKill{{ID: "newly-fetched-event", Timestamp: metrics.EvaluationTime}}
			d.AddMetrics(row, metrics, nil)
			if !strings.Contains(d.ChartStatus, "Changed") {
				t.Fatal(d.ChartStatus)
			}
		}
		if len(d.Charts) != 1 || d.Charts[0].Resource != "memory" || len(d.OOMEvents) != 1 {
			t.Fatalf("%s: missing saved memory timeline or leaked old-rollout event: %+v", stage, d.Charts)
		}
		memory := d.Charts[0]
		oomEvents, floors := 0, 0
		for _, event := range memory.Events {
			if event.Kind == "oom" {
				oomEvents++
				if event.Time != metrics.EvaluationTime-300000 || !strings.Contains(event.Label, "checkout-abc") || !strings.Contains(event.Label, "limit at termination 50 MiB") {
					t.Fatalf("saved OOM identity/time changed: %+v", event)
				}
			}
		}
		for _, line := range memory.Lines {
			if line.Style == "oom" && line.Value == 75 {
				floors++
			}
			if line.Style == "aggregate" || line.Style == "recommended" && line.Value != 100 {
				t.Fatalf("chart invented usage or recomputed the decision: %+v", line)
			}
		}
		if oomEvents != 1 || floors != 1 {
			t.Fatalf("missing OOM marker/floor: %+v", memory)
		}
		for _, index := range []int{2, 3} {
			card := d.Cards[index]
			if card.Recommended != "100Mi" || !strings.Contains(card.Steps[0].Detail, "50 MiB limit at termination × 1.5 = 75 MiB") || strings.Contains(card.Source, "series") {
				t.Fatalf("incomplete OOM calculation: %+v", card)
			}
		}
		text := d.Text(true)
		for _, want := range []string{"75 MiB is below the configured minimum of 100 MiB → 100 MiB", "100 MiB request × 1 = 100 MiB limit candidate", "usage freshness were not required", "Missing usage is not an observed zero"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s: missing %q", stage, want)
			}
		}
		page, err := HTML(d)
		if err != nil || !strings.Contains(string(page), "Recorded OOMKilled events") || !strings.Contains(string(page), `"kind":"oom"`) {
			t.Fatalf("%s: missing HTML event: %v", stage, err)
		}
	}
	after, err := json.Marshal(row)
	if err != nil || string(before) != string(after) {
		t.Fatal("explanation changed the saved scan")
	}
}

func TestUnknownOOMLimitWithoutUsageIsNoAction(t *testing.T) {
	run, row, metrics, result := oomFixtureResult(t, false)
	memory := result.Analysis.Results[0]
	if memory.DataQuality.Status != analysis.DataQualityUnavailable || memory.NoActionReason == "" || !slices.Contains(memory.DataQuality.Reasons, analysis.ReasonMissingUsage) || !slices.Contains(memory.DataQuality.Reasons, analysis.ReasonOOMLimitUnknown) || memory.OOMAdjustment != nil || len(memory.Recommendations) != 0 || len(memory.Evidence.OOMKills) != 1 || memory.Evidence.OOMKills[0].MemoryLimitBytes != 0 {
		t.Fatalf("unknown event-time limit without usage must be a no-action decision: %+v", memory)
	}
	d := Build(run, row)
	for _, stage := range []string{"offline", "empty fetch"} {
		if stage == "empty fetch" {
			d.AddMetrics(row, metrics, []string{"NoPrometheusMemoryUsage"})
			if !strings.Contains(d.ChartStatus, "No historical usage samples") {
				t.Fatal(d.ChartStatus)
			}
		}
		if len(d.OOMEvents) != 1 || !strings.Contains(d.OOMEvents[0].Label, "limit at termination unknown") {
			t.Fatalf("%s: unknown OOM event missing: %+v", stage, d.OOMEvents)
		}
		memoryCharts := 0
		for _, chart := range d.Charts {
			if chart.Resource != "memory" {
				continue
			}
			memoryCharts++
			oomEvents := 0
			for _, event := range chart.Events {
				if event.Kind == "oom" {
					oomEvents++
				}
			}
			for _, line := range chart.Lines {
				if line.Style == "oom" || line.Style == "aggregate" {
					t.Fatalf("%s: chart invented an OOM floor or usage: %+v", stage, line)
				}
			}
			if oomEvents != 1 {
				t.Fatalf("%s: memory chart lost the OOM marker: %+v", stage, chart)
			}
		}
		if memoryCharts != 1 {
			t.Fatalf("%s: missing memory timeline: %+v", stage, d.Charts)
		}
		for _, card := range d.Cards {
			if card.Outcome == "recommended" {
				t.Fatalf("%s: unknown OOM limit produced a recommendation: %+v", stage, card)
			}
			for _, step := range card.Steps {
				if strings.Contains(step.Detail, "(fallback)") {
					t.Fatalf("%s: unknown OOM limit borrowed a fallback: %+v", stage, card)
				}
			}
		}
		if text := d.Text(true); text == "" || strings.Contains(text, "(fallback)") || strings.Contains(text, "fallback sizing applies") {
			t.Fatalf("%s: markdown missing or used a fallback: %s", stage, text)
		}
		page, err := HTML(d)
		if err != nil || !strings.Contains(string(page), "Recorded OOMKilled events") || !strings.Contains(string(page), `"kind":"oom"`) || strings.Contains(string(page), "fallback sizing applies") {
			t.Fatalf("%s: missing HTML event: %v", stage, err)
		}
	}
}

func TestOOMKnownLimitCalculationAndChart(t *testing.T) {
	run, row, metrics := fixture(t)
	d := Build(run, row)
	d.AddMetrics(row, metrics, nil)
	if !strings.Contains(d.Cards[2].Steps[0].Detail, "256 MiB limit at termination × 1.5 = 384 MiB") {
		t.Fatal(d.Cards[2].Steps)
	}
	if len(d.OOMEvents) != 1 || !strings.Contains(d.OOMEvents[0].Label, "limit at termination 256 MiB") {
		t.Fatal(d.OOMEvents)
	}
}

func TestFetchEmptyUsageWithSavedOOM(t *testing.T) {
	_, row, _ := oomFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cfg := config.Default("simple")
	cfg.PrometheusURL, cfg.UseOOMKillData = &server.URL, true
	metrics, _, err := Fetch(context.Background(), row, cfg)
	if err != nil || len(metrics.Memory) != 0 || len(metrics.CPU) != 0 {
		t.Fatalf("OOM-only scan should allow an empty usage response: %+v, %v", metrics, err)
	}
	row.Scan.Analysis = nil
	if _, _, err = Fetch(context.Background(), row, cfg); err == nil {
		t.Fatal("missing ordinary usage must still be reported without saved OOM evidence")
	}
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/recommender/analysis"
)

func TestExplainSavedRunCLI(t *testing.T) {
	store := runstore.Store{Root: filepath.Join(t.TempDir(), "runs")}
	allocations := model.EmptyAllocations()
	allocations.Requests[model.Memory] = model.Unknown()
	context := "saved-production-context"
	scan := model.Scan{Object: model.Object{Name: "saved-workload", Namespace: "saved-namespace", Container: "main", Kind: "Deployment", Allocations: allocations}, Recommended: model.Recommendation{Requests: map[model.ResourceType]model.RecommendationValue{}, Limits: map[model.ResourceType]model.RecommendationValue{}}}
	run, err := store.Save(runstore.Run{Strategy: "simple", Rows: []runstore.Row{{Scan: scan, Connection: runstore.Connection{Context: &context}, WindowStart: 1000, EvaluationTime: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(args ...string) (string, error) {
		cmd := newExplainCommand(func() (runstore.Store, error) { return store, nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	text, err := execute("1", "--offline")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, context) || !strings.Contains(text, "saved-workload") || !strings.Contains(text, "file://") || strings.Contains(text, "\x1b") {
		t.Fatalf("output: %s", text)
	}
	html, err := os.ReadFile(filepath.Join(store.Root, run.ID, "explain-1.html"))
	if err != nil || !strings.Contains(string(html), "Offline") {
		t.Fatalf("HTML: %v", err)
	}
	text, err = execute("1", "--format", "text", "--run", run.ID)
	if err != nil || !strings.Contains(text, "unknown") || strings.Contains(text, "Fetching") {
		t.Fatalf("text: %s %v", text, err)
	}
	for _, args := range [][]string{{"0"}, {"-1"}, {"invalid"}, {"2"}, {"1", "--run", "../escape"}, {"1", "--format", "pdf"}, {"1", "--format", "text", "--output", "unused"}} {
		if _, err := execute(args...); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
}

func TestExplainOOMChartsSurviveOfflineAndFetchFailure(t *testing.T) {
	store := runstore.Store{Root: t.TempDir()}
	a := model.EmptyAllocations()
	a.Requests[model.Memory], a.Limits[model.Memory] = model.Number(50*1024*1024), model.Number(50*1024*1024)
	scan := model.Scan{
		Object:      model.Object{Name: "memory-leak", Namespace: "keda", Container: "main", Kind: model.StandalonePodKind, Allocations: a},
		Recommended: model.Recommendation{Requests: map[model.ResourceType]model.RecommendationValue{model.Memory: {Value: model.Number(100 * 1024 * 1024)}}, Limits: map[model.ResourceType]model.RecommendationValue{model.Memory: {Value: model.Number(100 * 1024 * 1024)}}},
		Analysis:    &analysis.Output{Results: []analysis.ResourceAnalysis{{Resource: analysis.ResourceMemory, Evidence: analysis.ResourceEvidence{OOMKills: []analysis.OOMKill{{ID: "kill", Timestamp: 1500}}}}}},
	}
	run, err := store.Save(runstore.Run{Strategy: "simple", Rows: []runstore.Row{{Scan: scan, WindowStart: 1000, EvaluationTime: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, offline := range []bool{true, false} {
		cmd := newExplainCommand(func() (runstore.Store, error) { return store, nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		args := []string{"1"}
		if offline {
			args = append(args, "--offline")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		page, err := os.ReadFile(filepath.Join(store.Root, run.ID, "explain-1.html"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"kind":"oom"`, "Recorded OOMKilled events", "Request at scan", "Recommended limit"} {
			if !strings.Contains(string(page), want) {
				t.Fatalf("offline=%t: lost %s", offline, want)
			}
		}
		if !offline && !strings.Contains(string(page), "Saved OOM events and allocation reference lines remain visible") {
			t.Fatal("fetch failure hid the saved OOM timeline")
		}
	}
}

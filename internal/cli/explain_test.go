package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/runstore"
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

package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/recommender/analysis"
	"github.com/kedify/recommender/reviewpatch"
)

func TestExportPatchOOMWithHeldCPU(t *testing.T) {
	const mi = 1024 * 1024
	target := analysis.Target{Namespace: "default", Kind: "Deployment", Name: "oom-example", Container: "app", WorkloadUID: "test-uid", Release: "pod-template-hash:1"}
	cluster := "test-cluster"
	store := runstore.Store{Root: t.TempDir()}
	identity := analysis.CurrentIdentity{Available: true, WorkloadUID: target.WorkloadUID, Release: target.Release}
	analysisOutput := &analysis.Output{
		SchemaVersion: analysis.OutputSchemaVersion, PolicyVersion: "test-policy", DetectorVersion: "9",
		Results: []analysis.ResourceAnalysis{
			{Target: target, Resource: analysis.ResourceCPU,
				Recommendations: []analysis.Recommendation{{Setting: analysis.SettingRequests, CurrentValue: 100, SuggestedValue: 200}}},
			{Target: target, Resource: analysis.ResourceMemory,
				Evidence: analysis.ResourceEvidence{
					Identity: identity, CurrentRequest: analysis.Signal{Available: true, Value: 8 * mi},
					CurrentLimit: analysis.Signal{Available: true, Value: 64 * mi},
				},
				OOMAdjustment: &analysis.OOMAdjustment{OOMRequestFloorBytes: 96 * mi},
				Recommendations: []analysis.Recommendation{
					{Setting: analysis.SettingRequests, CurrentValue: 8 * mi, SuggestedValue: 100 * mi},
					{Setting: analysis.SettingLimits, CurrentValue: 64 * mi, SuggestedValue: 100 * mi},
				}},
		},
	}
	_, err := store.Save(runstore.Run{Rows: []runstore.Row{{
		EvaluationTime: 1234, Identity: runstore.Identity{UID: target.WorkloadUID, Release: target.Release},
		Scan: model.Scan{
			Object:   model.Object{Cluster: &cluster, Namespace: target.Namespace, Kind: target.Kind, Name: target.Name, Container: target.Container},
			Analysis: analysisOutput, SuppressedResources: map[model.ResourceType]string{model.CPU: "HPA detected"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd := newExportPatchCommand(func() (runstore.Store, error) { return store, nil })
	cmd.SetArgs([]string{"1"})
	cmd.SetOut(&stdout)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result reviewpatch.Review
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := result.Document.Verify(); err != nil {
		t.Fatal(err)
	}
	artifact := result.Document.Artifact
	if len(artifact.Changes) != 1 || artifact.Changes[0].Resource != analysis.ResourceMemory ||
		*artifact.Changes[0].After.Request != 100*mi || *artifact.Changes[0].After.Limit != 100*mi ||
		len(artifact.Holds) != 1 || artifact.Holds[0].Resource != analysis.ResourceCPU {
		t.Fatalf("unexpected review: %+v", artifact)
	}
	for _, check := range result.Checks {
		if check.Status != reviewpatch.Unperformed {
			t.Fatalf("offline command claims a live check: %+v", check)
		}
	}
}

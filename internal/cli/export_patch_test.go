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
	metricCluster := "saved-metric-cluster"
	store := runstore.Store{Root: t.TempDir()}
	identity := analysis.CurrentIdentity{Available: true, WorkloadUID: target.WorkloadUID, Release: target.Release}
	analysisOutput := &analysis.Output{
		SchemaVersion: analysis.OutputSchemaVersion, PolicyVersion: "test-policy", DetectorVersion: "9",
		Results: []analysis.ResourceAnalysis{
			{Target: target, Resource: analysis.ResourceCPU,
				Evidence: analysis.ResourceEvidence{
					Identity: identity, CurrentRequest: analysis.Signal{Available: true, Value: 1001.0000000000001},
					CurrentLimit: analysis.Signal{Available: true, Unset: true},
				},
				Recommendations: []analysis.Recommendation{{Setting: analysis.SettingRequests, CurrentValue: 1000.9999999999999, SuggestedValue: 1100}}},
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
	row := runstore.Row{
		Connection:     runstore.Connection{ClusterValue: &metricCluster},
		EvaluationTime: 1234, Identity: runstore.Identity{UID: target.WorkloadUID, Release: target.Release},
		Scan: model.Scan{
			Object:   model.Object{Cluster: &cluster, Namespace: target.Namespace, Kind: target.Kind, Name: target.Name, Container: target.Container},
			Analysis: analysisOutput, SuppressedResources: map[model.ResourceType]string{model.CPU: "HPA detected"},
		},
	}
	unheld := row
	unheld.Scan.SuppressedResources = nil
	unsupported := row
	unsupported.Scan.Object.Kind = "StatefulSet"
	_, err := store.Save(runstore.Run{Rows: []runstore.Row{row, unheld, unsupported}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd := newExportPatchCommand(func() (runstore.Store, error) { return store, nil })
	cmd.SetArgs([]string{"1", "--cluster-id", "registered-cluster"})
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
	if artifact.Cluster != "registered-cluster" || len(artifact.Changes) != 1 || artifact.Changes[0].Resource != analysis.ResourceMemory ||
		*artifact.Changes[0].After.Request != 100*mi || *artifact.Changes[0].After.Limit != 100*mi ||
		len(artifact.Holds) != 1 || artifact.Holds[0].Resource != analysis.ResourceCPU {
		t.Fatalf("unexpected review: %+v", artifact)
	}
	if result.Checks == nil || len(result.Checks) != 0 || result.CheckedAt != 0 {
		t.Fatalf("offline export must have an empty check list and no live check time: %+v", result)
	}

	stdout.Reset()
	result = reviewpatch.Review{}
	cmd = newExportPatchCommand(func() (runstore.Store, error) { return store, nil })
	cmd.SetArgs([]string{"2"})
	cmd.SetOut(&stdout)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := result.Document.Verify(); err != nil {
		t.Fatal(err)
	}
	if result.Document.Artifact.Cluster != metricCluster {
		t.Fatalf("expected saved metric cluster, got %q", result.Document.Artifact.Cluster)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, result.Document.Artifact.Patch); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(compact.Bytes(), []byte(`"cpu":"1100m"`)) {
		t.Fatalf("missing unheld CPU patch: %s", compact.Bytes())
	}

	stdout.Reset()
	cmd = newExportPatchCommand(func() (runstore.Store, error) { return store, nil })
	cmd.SetArgs([]string{"3"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	if err := cmd.Execute(); err == nil || err.Error() != "row 3 is a StatefulSet. Patch export requires a Deployment" {
		t.Fatalf("unsupported workload error = %v", err)
	}
}

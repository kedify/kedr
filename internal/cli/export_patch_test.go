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
	legacy := unheld
	legacyOut := *analysisOutput
	legacyOut.Results = append([]analysis.ResourceAnalysis(nil), legacyOut.Results...)
	legacyCPU := &legacyOut.Results[0]
	legacyCPU.Evidence.CurrentLimit = analysis.Signal{Available: true, Value: 2000}
	legacyCPU.Recommendations = []analysis.Recommendation{{Setting: analysis.SettingLimits, CurrentValue: 2000, SuggestedValue: 1000}}
	legacy.Scan.Analysis = &legacyOut
	unsupported := row
	unsupported.Scan.Object.Kind = "StatefulSet"
	_, err := store.Save(runstore.Run{Rows: []runstore.Row{row, unheld, unsupported, legacy}})
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(result.Checks) != 4 || fields["checkedAt"] != nil {
		t.Fatalf("offline export returned checks %s and checkedAt %s", fields["checks"], fields["checkedAt"])
	}

	for _, check := range result.Checks {
		if check.Status != reviewpatch.Unperformed {
			t.Fatalf("offline check reported as %s: %v", check.Status, check)
		}
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

	stdout.Reset()
	cmd = newExportPatchCommand(func() (runstore.Store, error) { return store, nil })
	cmd.SetArgs([]string{"4"})
	cmd.SetOut(&stdout)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	result = reviewpatch.Review{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	artifact = result.Document.Artifact
	if len(artifact.Holds) != 1 || artifact.Holds[0].Resource != analysis.ResourceCPU || len(artifact.Changes) != 1 || artifact.Changes[0].Resource != analysis.ResourceMemory {
		t.Fatalf("legacy unguarded CPU cut must preserve independent memory increase: %+v", artifact)
	}
}

func TestLegacyZeroLimitCannotAuthorizeFirstCap(t *testing.T) {
	out := &analysis.Output{DetectorVersion: "9"}
	result := analysis.ResourceAnalysis{Resource: analysis.ResourceCPU, Evidence: analysis.ResourceEvidence{CurrentLimit: analysis.Signal{Available: true, Value: 0}}, Recommendations: []analysis.Recommendation{{Setting: analysis.SettingLimits, CurrentValue: 0, SuggestedValue: 100}}}
	if !lacksSavedGuards(out, result) {
		t.Fatal("legacy zero limit accepted a first finite cap")
	}
}

func TestSavedMemoryGuards(t *testing.T) {
	out := &analysis.Output{DetectorVersion: analysis.ResourceRightSizeDetectorVersion}
	res := analysis.ResourceAnalysis{Resource: analysis.ResourceMemory, Recommendations: []analysis.Recommendation{{Setting: analysis.SettingRequests, CurrentValue: 256, SuggestedValue: 128}}}
	if !lacksSavedGuards(out, res) {
		t.Fatal("memory decrease accepted without saved guards")
	}
	out.EffectivePolicy.Memory.SafetyGuards = true
	if lacksSavedGuards(out, res) {
		t.Fatal("guarded memory decrease held")
	}
	res.Recommendations[0].SuggestedValue = 512
	res.OOMAdjustment = &analysis.OOMAdjustment{UsedCurrentFallback: true}
	if !lacksSavedGuards(out, res) {
		t.Fatal("current-limit fallback used as a historical OOM floor")
	}
}

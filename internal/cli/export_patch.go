package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/recommender/analysis"
	"github.com/kedify/recommender/reviewpatch"
	"github.com/spf13/cobra"
)

func newExportPatchCommand(openStore func() (runstore.Store, error)) *cobra.Command {
	var runID, clusterID string
	cmd := &cobra.Command{
		Use:   "export-patch <row>",
		Short: "Export a Deployment container resource patch for review from a saved run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[0])
			if err != nil || number < 1 {
				return errors.New("row must be a positive integer")
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			run, err := store.Load(runID)
			if err != nil {
				return err
			}
			if number > len(run.Rows) {
				return fmt.Errorf("row %d is out of range; run %s has %d rows", number, run.ID, len(run.Rows))
			}
			row := run.Rows[number-1]
			object := row.Object()
			if object.Kind != "Deployment" {
				return fmt.Errorf("row %d is a %s. Patch export requires a Deployment", number, object.Kind)
			}
			cluster := object.Cluster
			if row.Connection.ClusterValue != nil && *row.Connection.ClusterValue != "" {
				cluster = row.Connection.ClusterValue
			}
			if clusterID != "" {
				cluster = &clusterID
			}
			if cluster == nil || *cluster == "" {
				return errors.New("saved row is missing cluster identity")
			}
			target := analysis.Target{
				Namespace: object.Namespace, Kind: object.Kind, Name: object.Name, Container: object.Container,
				WorkloadUID: object.UID, Release: object.Release,
			}
			holds := make(map[analysis.Resource]string, len(row.Scan.SuppressedResources))
			for resource, reason := range row.Scan.SuppressedResources {
				holds[analysis.Resource(resource)] = reason
			}
			requiresRescan := false
			if out := row.Scan.Analysis; out != nil {
				for _, result := range out.Results {
					if holds[result.Resource] == "" && lacksSavedGuards(out, result) {
						requiresRescan = true
						holds[result.Resource] = "Saved analysis lacks current safety checks for this change. Run a new scan."
					}
				}
			}
			doc, err := reviewpatch.Build(*cluster, "", target, row.EvaluationTime, row.Scan.Analysis, holds)
			if err != nil && requiresRescan {
				return fmt.Errorf("%w. Run a new scan to check changes held by the saved safety policy", err)
			}
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(reviewpatch.Review{Document: doc, Checks: reviewpatch.UnperformedChecks()})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "Saved run ID (defaults to the latest run)")
	cmd.Flags().StringVar(&clusterID, "cluster-id", "", "Registered cluster ID for the artifact (defaults to the saved scope)")
	return cmd
}

// A saved run may predate current membership and safety checks. Keep independent
// increases exportable, but require a new scan for changes that relied on them.
func lacksSavedGuards(out *analysis.Output, result analysis.ResourceAnalysis) bool {
	if adj := result.OOMAdjustment; adj != nil && (adj.UsedCurrentFallback || adj.UsedUsageFallback) {
		return true
	}
	for _, rec := range result.Recommendations {
		introducesCap := rec.Setting == analysis.SettingLimits && result.Evidence.CurrentLimit.Unset
		if !introducesCap && rec.SuggestedValue >= rec.CurrentValue {
			continue
		}
		if out.DetectorVersion != analysis.ResourceRightSizeDetectorVersion {
			return true
		}
		if result.Resource == analysis.ResourceMemory {
			return !out.EffectivePolicy.Memory.SafetyGuards
		}
		if rec.Setting == analysis.SettingLimits {
			guard := out.EffectivePolicy.CPU.LimitReductionGuard
			return guard == nil || !guard.RequireCompleteCoverage
		}
	}
	return false
}

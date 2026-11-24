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
	var runID string
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
			cluster := object.Cluster
			if row.Connection.ClusterValue != nil && *row.Connection.ClusterValue != "" {
				cluster = row.Connection.ClusterValue
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
			doc, err := reviewpatch.Build(*cluster, "", target, row.EvaluationTime, row.Scan.Analysis, holds)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(reviewpatch.Review{Document: doc, Checks: reviewpatch.UnperformedChecks()})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "Saved run ID (defaults to the latest run)")
	return cmd
}

package strategy

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kedify/recommender/analysis"
)

func TestClassificationDoesNotChangeSizingAndIncludesOOM(t *testing.T) {
	for _, strategy := range []string{"simple", "simple_limit"} {
		t.Run(strategy, func(t *testing.T) {
			cfg, metrics, object := fixture(4)
			cfg.Strategy = strategy
			metrics.OOMKills = []analysis.OOMKill{{ID: "oom", WorkloadUID: object.UID, Release: object.Release, PodUID: object.Pods[0].UID, Timestamp: metrics.EvaluationTime - 1000, MemoryLimitBytes: 2 * 1024 * testMiB}}
			before := run(t, cfg, metrics, object)
			cfg.ClassifyWorkloads = true
			after := run(t, cfg, metrics, object)
			if !reflect.DeepEqual(before.Resources, after.Resources) || !reflect.DeepEqual(before.Analysis.Results, after.Analysis.Results) {
				t.Fatal("classification changed resource sizing")
			}
			if !slices.Contains(after.Analysis.Classifications[0].Features, analysis.FeatureOOMKilled) {
				t.Fatal("flag alone did not collect OOM tag")
			}
			if after.Analysis.Results[0].MemoryLeak != nil || after.Analysis.Results[0].OOMAdjustment != nil {
				t.Fatal("classification enabled sizing flags")
			}
			cfg.UseOOMKillData = true
			cfg.DetectMemoryLeaks = true
			both := run(t, cfg, metrics, object)
			if both.Analysis.Results[0].OOMAdjustment == nil || both.Analysis.Results[0].MemoryLeak == nil {
				t.Fatal("existing flags no longer work")
			}
		})
	}
}

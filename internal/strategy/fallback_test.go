package strategy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

func rolloutFixture(eligible int) (*config.Config, Metrics, model.Object) {
	cfg, metrics, object := fixture(4)
	cfg.MinimumHistoryHours = 1
	cfg.PointsRequired = 100 // Exercise a caller's stricter sample-count policy.
	oldCPU, oldMemory := metrics.CPU[0], metrics.Memory[0]
	metrics.CPU[0].Samples = oldCPU.Samples[len(oldCPU.Samples)-21:]
	metrics.Memory[0].Samples = oldMemory.Samples[len(oldMemory.Samples)-21:]
	object.ReleaseStartedAt = metrics.Memory[0].Samples[0].Timestamp
	metrics.WindowStart -= 4 * 24 * 3600000
	object.Releases = []model.Release{{ID: object.Release, Name: "current", Current: true}}
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("old-%d", i)
		object.Releases = append(object.Releases, model.Release{ID: id, Name: fmt.Sprintf("rs-%d", i)})
		for _, original := range []analysis.Series{oldCPU, oldMemory} {
			old := original
			old.ID, old.PodUID, old.Release = id, id, id
			old.Samples = append([]analysis.Sample(nil), old.Samples...)
			if i != eligible {
				old.Samples = old.Samples[:90] // Over one hour, but fewer than 100 observations.
			}
			for j := range old.Samples {
				old.Samples[j].Timestamp -= int64(i) * 12 * 3600000
			}
			if old.Kind == analysis.SampleGauge {
				metrics.Memory = append(metrics.Memory, old)
			} else {
				metrics.CPU = append(metrics.CPU, old)
			}
		}
	}
	return cfg, metrics, object
}

func TestFallbackUsesThirdPreviousRolloutAndReportsSource(t *testing.T) {
	cfg, metrics, object := rolloutFixture(3)
	got := run(t, cfg, metrics, object)
	for _, result := range got.Analysis.Results {
		if result.RolloutFallback == nil || result.RolloutFallback.Release != "old-3" || result.Target.Release != object.Release {
			t.Fatalf("fallback did not reach third prior rollout: %+v", result)
		}
		resource := got.Resources[model.ResourceType(result.Resource)]
		if resource.Request.Unknown || resource.Info == nil || !strings.Contains(*resource.Info, "source: rs-3") || !strings.Contains(*resource.Info, "current rollout: insufficient-history") {
			t.Fatalf("fallback recommendation/provenance missing: %+v", resource)
		}
	}
	if got.ReleaseComparisons[0].Memory.HistoryHours > .34 || len(got.ReleaseComparisons[0].SizingResources) != 0 || len(got.ReleaseComparisons[3].SizingResources) != 2 {
		t.Fatalf("fallback usage was attributed to current rollout: %+v", got.ReleaseComparisons)
	}
	if got.Resources[model.CPU].Request.Value < .199 || got.Resources[model.CPU].Request.Value > .201 {
		t.Fatal("fallback changed CPU units")
	}
}

func TestFallbackStopsAfterThreePreviousRollouts(t *testing.T) {
	cfg, metrics, object := rolloutFixture(4)
	got := run(t, cfg, metrics, object)
	for _, result := range got.Analysis.Results {
		if result.RolloutFallback != nil || !got.Resources[model.ResourceType(result.Resource)].Request.Unknown {
			t.Fatalf("fourth prior rollout was used: %+v", result)
		}
	}
	cfg, metrics, object = rolloutFixture(3)
	cfg.ReleaseHistory = 1
	got = run(t, cfg, metrics, object)
	if !got.Resources[model.CPU].Request.Unknown || !got.Resources[model.Memory].Request.Unknown {
		t.Fatal("release-history=1 failed to disable fallback")
	}
}

func TestFallbackHonorsHPAAndRejectsUnverifiedImageCohorts(t *testing.T) {
	cfg, metrics, object := rolloutFixture(1)
	cpuTarget := 80.0
	object.HPA = &model.HPA{TargetCPUPercent: &cpuTarget}
	got := run(t, cfg, metrics, object)
	if !got.Resources[model.CPU].Request.Unknown || got.SuppressedResources[model.CPU] != "HPA detected" || got.Resources[model.Memory].Request.Unknown {
		t.Fatal("fallback bypassed HPA suppression or suppressed unrelated memory")
	}
	object.Releases[1].ID = "image:unverified"
	for _, rows := range [][]analysis.Series{metrics.CPU, metrics.Memory} {
		rows[1].Release = "image:unverified"
	}
	got = run(t, cfg, metrics, object)
	if !got.Resources[model.Memory].Request.Unknown {
		t.Fatal("unverified image cohort was used for fallback")
	}
}

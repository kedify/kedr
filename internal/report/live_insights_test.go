package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/model"
)

func TestLiveEvidenceIsVisibleAndExcludedFromSavedScan(t *testing.T) {
	count := int32(1)
	scan := model.Scan{Object: model.Object{
		Release: "current", HPA: &model.HPA{Live: []model.HPAObservation{
			{Name: "hpa-one-live", Owner: "standalone", CurrentReplicas: &count, MinReplicas: 1, MaxReplicas: 3},
			{Name: "hpa-two-live", Owner: "standalone", MinReplicas: 1, MaxReplicas: 3},
		}}, LivePodFailures: []model.PodFailureObservation{
			{PodName: "pod-one-live", Release: "current", Restarts: &count, TerminationReason: "OOMKilled", TerminatedAt: 12345},
			{PodName: "old-pod-live", Release: "old", Restarts: &count},
		},
	}}
	text := liveEvidenceNotes(scan.Object)
	for _, token := range []string{"Multiple HPAs", "hpa-one-live", "hpa-two-live", "pod-one-live restarts 1", "OOMKilled", "desired unknown"} {
		if !strings.Contains(text, token) {
			t.Fatalf("missing %q in %q", token, text)
		}
	}
	if strings.Contains(text, "old-pod-live") {
		t.Fatalf("older release leaked into observations: %q", text)
	}
	saved, err := json.Marshal(scan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "-live") {
		t.Fatalf("live evidence leaked into persisted scan: %s", saved)
	}
}

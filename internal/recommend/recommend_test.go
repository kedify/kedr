package recommend

import (
	"testing"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
)

func TestPreservesAnalyzerValuesAndSeverity(t *testing.T) {
	object := model.Object{Allocations: model.EmptyAllocations()}
	object.Allocations.Requests[model.CPU] = model.Number(1)
	object.Allocations.Requests[model.Memory] = model.Number(1024 * 1024 * 1024)
	raw := strategy.Result{Resources: map[model.ResourceType]strategy.RawRecommendation{model.CPU: {Request: model.Number(.0011), Limit: model.Unset()}, model.Memory: {Request: model.Number(1), Limit: model.Number(1)}}}
	scan := Scan(object, raw)
	if got := scan.Recommended.Requests[model.CPU].Value.Value; got != .0011 {
		t.Fatalf("CPU recommendation was changed: %v", got)
	}
	if got := scan.Recommended.Requests[model.Memory].Value.Value; got != 1 {
		t.Fatalf("memory recommendation was changed: %v", got)
	}
	if scan.Severity != model.SeverityCritical {
		t.Fatalf("severity=%s", scan.Severity)
	}
}

package recommend

import (
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
)

func TestRoundingFloorsAndSeverity(t *testing.T) {
	cfg := config.Default("simple")
	object := model.Object{Allocations: model.EmptyAllocations()}
	object.Allocations.Requests[model.CPU] = model.Number(1)
	object.Allocations.Requests[model.Memory] = model.Number(1024 * 1024 * 1024)
	raw := strategy.Result{model.CPU: {Request: model.Number(.0011), Limit: model.Unset()}, model.Memory: {Request: model.Number(1), Limit: model.Number(1)}}
	scan := Scan(cfg, object, raw)
	if got := scan.Recommended.Requests[model.CPU].Value.Value; got != .01 {
		t.Fatalf("CPU floor=%v", got)
	}
	if got := scan.Recommended.Requests[model.Memory].Value.Value; got != 100*1024*1024 {
		t.Fatalf("memory floor=%v", got)
	}
	if scan.Severity != model.SeverityCritical {
		t.Fatalf("severity=%s", scan.Severity)
	}
}

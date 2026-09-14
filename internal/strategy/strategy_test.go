package strategy

import (
	"math"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

func TestPercentileMatchesNumpyLinearInterpolation(t *testing.T) {
	got := percentile([]float64{1, 2, 3, 4}, 66)
	if math.Abs(got-2.98) > 1e-12 {
		t.Fatalf("percentile=%v", got)
	}
}

func TestSimpleRecommendation(t *testing.T) {
	cfg := config.Default("simple")
	cfg.PointsRequired = 2
	metrics := Metrics{
		"PercentileCPULoader": {"pod": {{Value: .2}, {Value: .4}}}, "CPUAmountLoader": {"pod": {{Value: 2}}},
		"MaxMemoryLoader": {"pod": {{Value: 100}}}, "MemoryAmountLoader": {"pod": {{Value: 2}}},
	}
	got := Run(cfg, metrics, model.Object{})
	if got[model.CPU].Request.Value != .4 || got[model.CPU].Limit.Set {
		t.Fatalf("cpu=%+v", got[model.CPU])
	}
	if math.Abs(got[model.Memory].Request.Value-115) > 1e-12 {
		t.Fatalf("memory=%v", got[model.Memory].Request.Value)
	}
}

func TestHPAAndOOMKill(t *testing.T) {
	cfg := config.Default("simple")
	cfg.PointsRequired = 1
	cpu := 50.0
	object := model.Object{HPA: &model.HPA{TargetCPUPercent: &cpu}}
	metrics := Metrics{"PercentileCPULoader": {"p": {{Value: .2}}}, "CPUAmountLoader": {"p": {{Value: 1}}}, "MaxMemoryLoader": {"p": {{Value: 100}}}, "MemoryAmountLoader": {"p": {{Value: 1}}}, "MaxOOMKilledMemoryLoader": {"p": {{Value: 200}}}}
	cfg.UseOOMKillData = true
	got := Run(cfg, metrics, object)
	if !got[model.CPU].Request.Unknown {
		t.Fatal("HPA should suppress CPU")
	}
	if got[model.Memory].Request.Value != 250 || got[model.Memory].Info == nil {
		t.Fatalf("OOM result=%+v", got[model.Memory])
	}
}

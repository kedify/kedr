// Package strategy implements kedr's recommendation algorithms.
package strategy

import (
	"math"
	"sort"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

type Point struct{ Time, Value float64 }
type PodSeries map[string][]Point
type Metrics map[string]PodSeries

type RawRecommendation struct {
	Request model.MaybeValue
	Limit   model.MaybeValue
	Info    *string
}

type Result map[model.ResourceType]RawRecommendation

func Run(cfg *config.Config, metrics Metrics, object model.Object) Result {
	return Result{model.CPU: cpu(cfg, metrics, object), model.Memory: memory(cfg, metrics, object)}
}

func cpu(cfg *config.Config, metrics Metrics, object model.Object) RawRecommendation {
	seriesName := "PercentileCPULoader"
	if cfg.Strategy == "simple_limit" {
		seriesName = "CPULoader"
	}
	data := metrics[seriesName]
	if len(data) == 0 {
		return undefined("No data")
	}
	if pointCount(metrics["CPUAmountLoader"]) < float64(cfg.PointsRequired) {
		return undefined("Not enough data")
	}
	if object.HPA != nil && object.HPA.TargetCPUPercent != nil && !cfg.AllowHPA {
		return undefined("HPA detected")
	}
	values := flatten(data)
	if cfg.Strategy == "simple" {
		// PercentileCPULoader computes the percentile in PromQL; KRR then takes its maximum.
		return RawRecommendation{Request: model.Number(maxValue(values)), Limit: model.Unset()}
	}
	return RawRecommendation{Request: model.Number(percentile(values, cfg.CPURequest)), Limit: model.Number(percentile(values, cfg.CPULimit))}
}

func memory(cfg *config.Config, metrics Metrics, object model.Object) RawRecommendation {
	data := metrics["MaxMemoryLoader"]
	if len(data) == 0 {
		return undefined("No data")
	}
	if pointCount(metrics["MemoryAmountLoader"]) < float64(cfg.PointsRequired) {
		return undefined("Not enough data")
	}
	if object.HPA != nil && object.HPA.TargetMemoryPercent != nil && !cfg.AllowHPA {
		return undefined("HPA detected")
	}
	peak := 0.0
	for _, points := range data {
		for _, point := range points {
			if point.Value > peak {
				peak = point.Value
			}
		}
	}
	value := peak * (1 + cfg.MemoryBufferPercent/100)
	var info *string
	if cfg.UseOOMKillData {
		oom := maxValue(flatten(metrics["MaxOOMKilledMemoryLoader"]))
		if oom > 0 {
			value = math.Max(value, oom*(1+cfg.OOMMemoryBuffer/100))
			s := "OOMKill detected"
			info = &s
		}
	}
	return RawRecommendation{Request: model.Number(value), Limit: model.Number(value), Info: info}
}

func undefined(message string) RawRecommendation {
	return RawRecommendation{Request: model.Unknown(), Limit: model.Unknown(), Info: &message}
}

func pointCount(series PodSeries) float64 {
	total := 0.0
	for _, points := range series {
		if len(points) > 0 {
			total += points[0].Value
		}
	}
	return total
}

func flatten(series PodSeries) []float64 {
	values := make([]float64, 0)
	for _, points := range series {
		for _, point := range points {
			values = append(values, point.Value)
		}
	}
	return values
}

func maxValue(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	value := values[0]
	for _, candidate := range values[1:] {
		if candidate > value {
			value = candidate
		}
	}
	return value
}

// percentile matches numpy.percentile's default linear interpolation.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := p / 100 * float64(len(sorted)-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))
	if lower == upper {
		return sorted[lower]
	}
	weight := index - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

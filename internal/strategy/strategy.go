// Package strategy adapts kedr observations and report values to the shared analyzer.
// Recommendation calculations live exclusively in github.com/kedify/recommender.
package strategy

import (
	"fmt"
	"strings"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

type Point struct{ Time, Value float64 }

type Metrics struct {
	WindowStart    int64
	EvaluationTime int64
	CPU            []analysis.Series
	Memory         []analysis.Series
	OOMKills       []analysis.OOMKill
}

type RawRecommendation struct {
	Request model.MaybeValue
	Limit   model.MaybeValue
	Info    *string
}

type Result struct {
	Resources           map[model.ResourceType]RawRecommendation
	Analysis            *analysis.Output
	ReleaseComparisons  []model.ReleaseComparison
	SuppressedResources map[model.ResourceType]string
}

// Input preserves identity, units and observation timestamps. It does not invent
// historical identity or turn absent metrics into observed zeroes.
func Input(cfg *config.Config, metrics Metrics, object model.Object) analysis.Input {
	target := analysis.Target{Namespace: object.Namespace, Kind: object.Kind, Name: object.Name, Container: object.Container, WorkloadUID: object.UID, Release: object.Release}
	eligible := 0
	for _, pod := range object.Pods {
		if !pod.Deleted && pod.Release == object.Release {
			eligible++
		}
	}
	c := analysis.ContainerObservation{
		Target:   target,
		Identity: analysis.CurrentIdentity{Available: object.UID != "" && object.Release != "", WorkloadUID: object.UID, Release: object.Release, Timestamp: object.ObservedAt, ReleaseStartedAt: object.ReleaseStartedAt, ReleaseStartInferred: object.ReleaseStartInferred, Ambiguous: object.IdentityAmbiguous},
		// The analyzer independently compares each resource's fresh observed pods
		// to this API inventory; metric availability is not assumed here.
		Inventory: analysis.Inventory{Available: object.InventoryAvailable, Timestamp: object.ObservedAt, Eligible: eligible, Observed: eligible, Excluded: object.ExcludedPods},
		CPU:       analysis.ResourceObservation{Series: releaseSeries(metrics.CPU, object.Release)},
		Memory:    analysis.ResourceObservation{Series: releaseSeries(metrics.Memory, object.Release)},
	}
	for _, entry := range []struct {
		resource model.ResourceType
		obs      *analysis.ResourceObservation
	}{{model.CPU, &c.CPU}, {model.Memory, &c.Memory}} {
		entry.obs.CurrentRequest = currentSignal(object, entry.resource, false)
		entry.obs.CurrentLimit = currentSignal(object, entry.resource, true)
	}
	if cfg.UseOOMKillData {
		c.OOMKills = append(append([]analysis.OOMKill(nil), object.OOMKills...), metrics.OOMKills...)
	}
	return analysis.Input{SchemaVersion: analysis.InputSchemaVersion, WindowStart: metrics.WindowStart, EvaluationTime: metrics.EvaluationTime, Containers: []analysis.ContainerObservation{c}}
}

func currentSignal(object model.Object, resource model.ResourceType, limit bool) analysis.Signal {
	value := object.Allocations.Requests[resource]
	if limit {
		value = object.Allocations.Limits[resource]
	}
	signal := analysis.Signal{Available: !value.Unknown, Timestamp: object.ObservedAt}
	if value.Set {
		signal.Value = value.Value
	}
	for _, pod := range object.Pods {
		if pod.Deleted || pod.Release != object.Release {
			continue
		}
		actual := pod.Allocations.Requests[resource]
		if limit {
			actual = pod.Allocations.Limits[resource]
		}
		if actual.Unknown || actual.Set != value.Set || actual.Value != value.Value {
			signal.Available = false
		}
	}
	if resource == model.CPU {
		signal.Value *= 1000
	} // Report cores -> analyzer millicores.
	return signal
}

func Run(cfg *config.Config, metrics Metrics, object model.Object) (Result, error) {
	policy, err := cfg.AnalysisPolicy()
	if err != nil {
		return Result{}, err
	}
	out, err := analysis.Analyze(Input(cfg, metrics, object), policy)
	if err != nil {
		return Result{}, fmt.Errorf("analyze %s/%s/%s: %w", object.Namespace, object.Name, object.Container, err)
	}
	result := Result{Resources: make(map[model.ResourceType]RawRecommendation), Analysis: &out, SuppressedResources: make(map[model.ResourceType]string)}
	result.ReleaseComparisons, err = compareReleases(cfg, metrics, object, policy, out)
	if err != nil {
		return Result{}, err
	}
	for _, resource := range out.Results {
		kind := model.ResourceType(resource.Resource)
		value := RawRecommendation{Request: object.Allocations.Requests[kind], Limit: object.Allocations.Limits[kind]}
		if resource.DataQuality.Status == analysis.DataQualityUnavailable {
			value.Request, value.Limit = model.Unknown(), model.Unknown()
		} else {
			for _, rec := range resource.Recommendations {
				number := rec.SuggestedValue
				if kind == model.CPU {
					number /= 1000
				}
				if rec.Setting == analysis.SettingRequests {
					value.Request = model.Number(number)
				} else {
					value.Limit = model.Number(number)
				}
			}
		}
		var reasons []string
		for _, reason := range resource.DataQuality.Reasons {
			message := string(reason)
			if reason == analysis.ReasonInsufficientHistory {
				message += fmt.Sprintf(" (observed %.1fh; required %.1fh)", resource.DataQuality.ObservedIntervalHours, float64(out.EffectivePolicy.Evidence.MinimumHistorySeconds)/3600)
			}
			if reason == analysis.ReasonSparseCoverage {
				message += fmt.Sprintf(" (observed %.1f%%; required %.1f%%)", 100*resource.DataQuality.Coverage, 100*out.EffectivePolicy.Evidence.MinimumCoverage)
			}
			reasons = append(reasons, message)
		}
		for _, notice := range resource.Notices {
			reasons = append(reasons, string(notice))
		}
		if len(reasons) == 0 && resource.NoActionReason != "" {
			reasons = append(reasons, string(resource.NoActionReason))
		}
		if !cfg.AllowHPA && object.HPA != nil && (kind == model.CPU && object.HPA.TargetCPUPercent != nil || kind == model.Memory && object.HPA.TargetMemoryPercent != nil) {
			value.Request, value.Limit = model.Unknown(), model.Unknown()
			result.SuppressedResources[kind] = "HPA detected"
			reasons = append(reasons, "HPA detected")
		}
		if len(reasons) > 0 {
			message := strings.Join(reasons, "; ")
			value.Info = &message
		}
		result.Resources[kind] = value
	}
	return result, nil
}

func releaseSeries(series []analysis.Series, release string) []analysis.Series {
	var result []analysis.Series
	for _, row := range series {
		if row.Release == release {
			result = append(result, row)
		}
	}
	return result
}

func compareReleases(cfg *config.Config, metrics Metrics, object model.Object, policy analysis.Policy, current analysis.Output) ([]model.ReleaseComparison, error) {
	var comparisons []model.ReleaseComparison
	for _, release := range object.Releases {
		output := current
		if release.ID != object.Release {
			past := object
			past.Release, past.ReleaseStartedAt = release.ID, 0
			past.IdentityAmbiguous, past.ReleaseStartInferred = false, false
			pastMetrics := metrics
			pastMetrics.CPU, pastMetrics.Memory = releaseSeries(metrics.CPU, release.ID), releaseSeries(metrics.Memory, release.ID)
			last := int64(0)
			for _, rows := range [][]analysis.Series{pastMetrics.CPU, pastMetrics.Memory} {
				for _, row := range rows {
					for _, sample := range row.Samples {
						last = max(last, sample.Timestamp)
					}
				}
			}
			if last <= metrics.WindowStart {
				comparisons = append(comparisons, model.ReleaseComparison{Release: release})
				continue
			}
			pastMetrics.EvaluationTime, past.ObservedAt = last, last
			input := Input(cfg, pastMetrics, past)
			// Historical comparisons have no historical allocation/inventory
			// snapshot. Do not claim today's settings existed at that time and
			// do not emit actionable recommendations for old releases.
			input.Containers[0].Inventory = analysis.Inventory{}
			input.Containers[0].CPU.CurrentRequest, input.Containers[0].CPU.CurrentLimit = analysis.Signal{}, analysis.Signal{}
			input.Containers[0].Memory.CurrentRequest, input.Containers[0].Memory.CurrentLimit = analysis.Signal{}, analysis.Signal{}
			var err error
			output, err = analysis.Analyze(input, policy)
			if err != nil {
				return nil, fmt.Errorf("compare release %s: %w", release.ID, err)
			}
		}
		comparison := model.ReleaseComparison{Release: release}
		for _, result := range output.Results {
			quality := result.DataQuality
			usage := model.ReleaseUsage{AggregatedUsage: result.Evidence.AggregatedUsage, ObservedStart: quality.ObservedStart, ObservedEnd: quality.ObservedEnd, HistoryHours: quality.ObservedIntervalHours, Coverage: quality.Coverage, SampleCount: quality.SampleCount, OOMKills: len(result.Evidence.OOMKills)}
			if result.Resource == analysis.ResourceCPU {
				comparison.CPU = usage
			} else {
				comparison.Memory = usage
			}
		}
		comparisons = append(comparisons, comparison)
	}
	return comparisons, nil
}

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
	// Discovery orders and retains the current rollout plus up to three previous
	// rollouts. Supply each source separately; never pool or relabel its samples.
	remaining := min(analysis.MaxPreviousReleases, max(0, cfg.ReleaseHistory-1))
	for _, release := range object.Releases {
		if release.ID == object.Release {
			continue
		}
		if remaining == 0 {
			break
		}
		remaining--
		// Unmatched image cohorts do not establish a trustworthy rollout identity.
		if release.ID == "" || strings.HasPrefix(release.ID, "image:") {
			continue
		}
		previous := analysis.PreviousRelease{Release: release.ID, CPU: releaseSeries(metrics.CPU, release.ID), Memory: releaseSeries(metrics.Memory, release.ID)}
		for _, rows := range [][]analysis.Series{previous.CPU, previous.Memory} {
			for _, row := range rows {
				for _, sample := range row.Samples {
					if sample.Timestamp <= metrics.EvaluationTime {
						previous.EvaluationTime = max(previous.EvaluationTime, sample.Timestamp)
					}
				}
			}
		}
		c.PreviousReleases = append(c.PreviousReleases, previous)
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
	input := Input(cfg, metrics, object)
	out, err := analysis.Analyze(input, policy)
	if err != nil {
		return Result{}, fmt.Errorf("analyze %s/%s/%s: %w", object.Namespace, object.Name, object.Container, err)
	}
	result := Result{Resources: make(map[model.ResourceType]RawRecommendation), Analysis: &out, SuppressedResources: make(map[model.ResourceType]string)}
	current := out
	for _, resource := range out.Results {
		if resource.RolloutFallback != nil {
			input.Containers[0].PreviousReleases = nil
			current, err = analysis.Analyze(input, policy)
			if err != nil {
				return Result{}, err
			}
			break
		}
	}
	result.ReleaseComparisons, err = compareReleases(cfg, metrics, object, policy, current)
	if err != nil {
		return Result{}, err
	}
	for _, resource := range out.Results {
		if resource.DataQuality.Status == analysis.DataQualityUnavailable {
			continue
		}
		source := object.Release
		if resource.RolloutFallback != nil {
			source = resource.RolloutFallback.Release
		}
		for i := range result.ReleaseComparisons {
			if result.ReleaseComparisons[i].Release.ID == source {
				result.ReleaseComparisons[i].SizingResources = append(result.ReleaseComparisons[i].SizingResources, model.ResourceType(resource.Resource))
			}
		}
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
		reasons := qualityReasons(resource.DataQuality, out.EffectivePolicy)
		for _, notice := range resource.Notices {
			message := string(notice)
			if fallback := resource.RolloutFallback; notice == analysis.ReasonPreviousReleaseUsage && fallback != nil {
				name := fallback.Release
				for _, release := range object.Releases {
					if release.ID == name && release.Name != "" {
						name = release.Name
						break
					}
				}
				message += fmt.Sprintf(" (source: %s; current rollout: %s)", name, strings.Join(qualityReasons(fallback.CurrentDataQuality, out.EffectivePolicy), ", "))
			}
			reasons = append(reasons, message)
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

func qualityReasons(quality analysis.DataQuality, policy analysis.Policy) []string {
	var reasons []string
	for _, reason := range quality.Reasons {
		message := string(reason)
		if reason == analysis.ReasonInsufficientHistory {
			message += fmt.Sprintf(" (observed %.1fh; required %.1fh)", quality.ObservedIntervalHours, float64(policy.Evidence.MinimumHistorySeconds)/3600)
		}
		if reason == analysis.ReasonInsufficientSamples {
			message += fmt.Sprintf(" (observed %d; required %d)", quality.ObservationCount, policy.Evidence.MinimumSamples)
		}
		if reason == analysis.ReasonSparseCoverage {
			message += fmt.Sprintf(" (observed %.1f%%; required %.1f%%)", 100*quality.Coverage, 100*policy.Evidence.MinimumCoverage)
		}
		reasons = append(reasons, message)
	}
	return reasons
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
	for _, release := range object.Releases[:min(max(1, cfg.ReleaseHistory), analysis.MaxPreviousReleases+1, len(object.Releases))] {
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
			input.Containers[0].PreviousReleases = nil
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

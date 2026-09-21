// Package recommend turns raw strategy results into report scans.
package recommend

import (
	"math"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
)

func Scan(object model.Object, raw strategy.Result) model.Scan {
	recommendation := model.Recommendation{Requests: map[model.ResourceType]model.RecommendationValue{}, Limits: map[model.ResourceType]model.RecommendationValue{}, Info: map[model.ResourceType]*string{}}
	severities := make([]model.Severity, 0, 4)
	for _, resource := range model.ResourceTypes {
		value := raw.Resources[resource]
		request, limit := value.Request, value.Limit
		rqSeverity := severity(object.Allocations.Requests[resource], request, resource)
		limitSeverity := severity(object.Allocations.Limits[resource], limit, resource)
		recommendation.Requests[resource] = model.RecommendationValue{Value: request, Severity: rqSeverity}
		recommendation.Limits[resource] = model.RecommendationValue{Value: limit, Severity: limitSeverity}
		recommendation.Info[resource] = value.Info
		severities = append(severities, rqSeverity, limitSeverity)
	}
	return model.Scan{Object: object, Recommended: recommendation, Severity: model.WorstSeverity(severities...), Analysis: raw.Analysis, ReleaseComparisons: raw.ReleaseComparisons, SuppressedResources: raw.SuppressedResources}
}

func severity(current, recommended model.MaybeValue, resource model.ResourceType) model.Severity {
	if current.Unknown || recommended.Unknown {
		return model.SeverityUnknown
	}
	if !current.Set && !recommended.Set {
		return model.SeverityGood
	}
	if !current.Set || !recommended.Set {
		return model.SeverityWarning
	}
	diff := math.Abs(current.Value - recommended.Value)
	if resource == model.Memory {
		diff /= 1024 * 1024
		if diff >= 500 {
			return model.SeverityCritical
		}
		if diff >= 250 {
			return model.SeverityWarning
		}
		if diff >= 100 {
			return model.SeverityOK
		}
		return model.SeverityGood
	}
	if diff >= .5 {
		return model.SeverityCritical
	}
	if diff >= .25 {
		return model.SeverityWarning
	}
	if diff >= .1 {
		return model.SeverityOK
	}
	return model.SeverityGood
}

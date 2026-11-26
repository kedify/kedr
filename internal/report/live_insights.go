package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kedify/kedr/internal/model"
)

func liveEvidenceNotes(object model.Object) string {
	var notes []string
	for _, failure := range object.LivePodFailures {
		if failure.Release != object.Release || object.Release == "" {
			continue
		}
		if failure.Evicted {
			notes = append(notes, failure.PodName+" eviction observed")
		}
		if failure.Restarts == nil {
			continue
		}
		detail := fmt.Sprintf("%s restarts %d", failure.PodName, *failure.Restarts)
		if failure.TerminatedAt != 0 {
			detail += fmt.Sprintf(", last termination %s at %s", failure.TerminationReason, time.UnixMilli(failure.TerminatedAt).UTC().Format(time.RFC3339))
		}
		notes = append(notes, detail)
	}
	if object.HPA == nil || len(object.HPA.Live) == 0 {
		return strings.Join(notes, "; ")
	}
	if len(object.HPA.Live) > 1 {
		notes = append(notes, "Multiple HPAs target this workload")
	}
	for _, hpa := range object.HPA.Live {
		notes = append(notes, fmt.Sprintf("HPA %s (%s), replicas current %s / desired %s / min %d / max %d",
			hpa.Name, hpa.Owner, knownReplicas(hpa.CurrentReplicas), knownReplicas(hpa.DesiredReplicas), hpa.MinReplicas, hpa.MaxReplicas))
		for _, metric := range hpa.Metrics {
			scope := metric.Type
			if metric.Container != "" {
				scope += " " + metric.Container
			}
			format := tableQuantity(metric.Resource)
			if metric.TargetType == "Utilization" {
				format = func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) + "%" }
			}
			current := "unknown"
			if metric.Current != nil {
				current = format(*metric.Current)
			}
			notes = append(notes, fmt.Sprintf("%s %s %s, current %s / target %s", scope, metric.Resource, metric.TargetType, current, format(metric.Target)))
		}
	}
	return strings.Join(notes, "; ")
}

func knownReplicas(value *int32) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(int(*value))
}

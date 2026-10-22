package explain

import (
	"fmt"
	"math"
	"sort"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/recommender/analysis"
)

func oomOnly(a *analysis.ResourceAnalysis) bool {
	return a.OOMAdjustment != nil && a.DecisionTrace != nil && a.DecisionTrace.Source.Method == "OOM kill"
}

// Explain the saved floor and effective multiplier, without running the sizing
// policy again or treating a current setting as a historical failed limit.
func oomCalculation(a *analysis.ResourceAnalysis, coefficient float64) string {
	adjustment := a.OOMAdjustment
	if adjustment == nil {
		return ""
	}
	floor := native(model.Memory, adjustment.OOMRequestFloorBytes)
	if coefficient <= 0 {
		return "Saved OOM floor: " + floor + ". The multiplier was not recorded."
	}
	base := adjustment.OOMRequestFloorBytes / coefficient
	source := "memory sizing base"
	if adjustment.UsedCurrentFallback {
		source = "current memory setting (fallback)"
	} else if adjustment.UsedUsageFallback {
		source = "usage baseline (fallback)"
	}
	for _, event := range a.Evidence.OOMKills {
		if event.MemoryLimitBytes > 0 && math.Abs(event.MemoryLimitBytes-base) <= math.Max(1e-6, math.Abs(base)*1e-12) {
			source = "limit at termination"
			break
		}
	}
	return fmt.Sprintf("%s %s × %g = %s OOM floor. The strongest event sets the floor; repeated events do not compound the multiplier.", native(model.Memory, base), source, coefficient, floor)
}

func savedOOMEvents(row runstore.Row) []Event {
	var events []Event
	if row.Scan.Analysis == nil {
		return events
	}
	kills := make(map[string]analysis.OOMKill)
	for _, result := range row.Scan.Analysis.Results {
		if result.Resource == analysis.ResourceMemory {
			for _, kill := range result.Evidence.OOMKills {
				kills[kill.ID] = kill
			}
		}
	}
	if c := row.Scan.Classification(); c != nil {
		for _, finding := range c.Findings {
			for _, kill := range finding.OOMKills {
				kills[kill.ID] = kill
			}
		}
	}
	ordered := make([]analysis.OOMKill, 0, len(kills))
	for _, kill := range kills {
		ordered = append(ordered, kill)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Timestamp != ordered[j].Timestamp {
			return ordered[i].Timestamp < ordered[j].Timestamp
		}
		return ordered[i].ID < ordered[j].ID
	})
	for _, kill := range ordered {
		pod := kill.PodUID
		for _, p := range row.Identity.Pods {
			if p.UID == kill.PodUID {
				pod = p.Name
				break
			}
		}
		label := "OOMKilled"
		if pod != "" {
			label += " · " + pod
		}
		if kill.MemoryLimitBytes > 0 {
			label += " · limit at termination " + native(model.Memory, kill.MemoryLimitBytes)
		} else {
			label += " · limit at termination unknown"
		}
		events = append(events, Event{Time: kill.Timestamp, Label: label, Kind: "oom"})
	}
	return events
}

func (e Event) When() string { return instant(e.Time) }

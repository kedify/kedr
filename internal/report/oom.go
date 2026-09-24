package report

import (
	"fmt"
	"time"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

// Use the analyzer's selected events, so the column agrees with the decision
// and cannot include another rollout or an event outside the scan window.
func latestOOM(scan model.Scan) int64 {
	var latest int64
	if scan.Analysis != nil {
		for _, result := range scan.Analysis.Results {
			if result.Resource == analysis.ResourceMemory {
				for _, kill := range result.Evidence.OOMKills {
					latest = max(latest, kill.Timestamp)
				}
			}
		}
	}
	return latest
}

func oomCell(timestamp int64, now time.Time) string {
	if timestamp <= 0 {
		return ""
	}
	event := time.UnixMilli(timestamp).In(now.Location())
	age := max(time.Duration(0), now.Sub(event))
	var when string
	switch {
	case age < time.Minute:
		when = fmt.Sprintf("%ds ago", int(age/time.Second))
	case age <= 15*time.Minute:
		when = fmt.Sprintf("%dm ago", int(age/time.Minute))
	default:
		when = event.Format("15:04:05")
		day := event.Format(time.DateOnly)
		if day == now.AddDate(0, 0, -1).Format(time.DateOnly) {
			when += " yesterday"
		} else if day != now.Format(time.DateOnly) {
			when += " " + day
		}
	}
	return "Yes (" + when + ")"
}

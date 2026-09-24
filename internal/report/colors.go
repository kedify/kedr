package report

import (
	"image/color"
	"math"

	"charm.land/lipgloss/v2"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/resource"
)

const diffGrey = "#929292"

type diffRange struct{ minimum, maximum float64 }

func (r *diffRange) include(magnitude float64) {
	if r.minimum == 0 || magnitude < r.minimum {
		r.minimum = magnitude
	}
	r.maximum = max(r.maximum, magnitude)
}

func (r diffRange) intensity(magnitude float64) float64 {
	// A lone value, or equal values, are all the largest change of that sign.
	if r.minimum == r.maximum {
		return 1
	}
	return (magnitude - r.minimum) / (r.maximum - r.minimum)
}

type diffColorScale struct {
	changes            []float64
	savings, increases diffRange
}

func newDiffColorScale(scans []model.Scan, visible []int, kind model.ResourceType) diffColorScale {
	scale := diffColorScale{changes: make([]float64, len(visible))}
	format := tableQuantity(kind)
	for row, index := range visible {
		scan := scans[index]
		current, next := scan.Object.Allocations.Requests[kind], scan.Recommended.Requests[kind].Value
		// Missing allocations are not observed zeroes. Displayed no-ops are neutral.
		if current.Unknown || !current.Set || next.Unknown || !next.Set || format(current.Value) == format(next.Value) {
			continue
		}
		change := (next.Value - current.Value) * float64(tableDiffMultiplier(scan, kind))
		if math.IsNaN(change) || math.IsInf(change, 0) {
			continue
		}
		// Equal displayed diffs get equal shades, even when subtraction leaves
		// tiny floating-point differences. Parse units back to a common scale.
		magnitude, err := resource.Parse(format(math.Abs(change)))
		if err != nil || magnitude == 0 {
			continue
		}
		change = math.Copysign(magnitude, change)
		scale.changes[row] = change
		if change < 0 {
			scale.savings.include(-change)
		} else if change > 0 {
			scale.increases.include(change)
		}
	}
	return scale
}

func (s diffColorScale) style(row int) lipgloss.Style {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(diffGrey))
	change := s.changes[row]
	if change == 0 {
		return style
	}
	var intensity float64
	var foreground color.Color
	if change < 0 {
		intensity = s.savings.intensity(-change)
		foreground = blendColor("#87958C", "#39FF88", intensity)
	} else {
		intensity = s.increases.intensity(change)
		if intensity < .5 {
			foreground = blendColor("#A39890", "#F5B75B", intensity*2)
		} else {
			foreground = blendColor("#F5B75B", "#FF3B4E", (intensity-.5)*2)
		}
	}
	return style.Foreground(foreground).Bold(intensity == 1)
}

func blendColor(from, to string, intensity float64) color.Color {
	r1, g1, b1, _ := lipgloss.Color(from).RGBA()
	r2, g2, b2, _ := lipgloss.Color(to).RGBA()
	channel := func(a, b uint32) uint8 {
		return uint8(math.Round(float64(a>>8)*(1-intensity) + float64(b>>8)*intensity))
	}
	return color.RGBA{R: channel(r1, r2), G: channel(g1, g2), B: channel(b1, b2), A: 255}
}

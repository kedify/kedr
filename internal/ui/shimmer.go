package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// Cyan from https://kedify.io/assets/images/kedify-logo-black-text.svg.
const progressColor = "#01CCD9"

// Adapted from github.com/handleui/shimmer, commit 27767358e0b0.
// Copyright (c) 2025 @handleui. See NOTICE for the MIT license.
// The wave matches the library's defaults: eight characters, 90% peak light,
// and eight positions of pause between passes. Frames are rendered once so the
// progress loop only needs to append the changing completion count.
func shimmerFrames(text string) []string {
	const waveWidth, wavePause = 8, 8
	base := lipgloss.NewStyle().Foreground(lipgloss.Color(progressColor))
	r, g, b, _ := lipgloss.Color(progressColor).RGBA()
	wave := make([]lipgloss.Style, waveWidth)
	mid := waveWidth / 2
	for i := range wave {
		ratio := float64(i) / float64(mid)
		if i > mid {
			ratio = float64(waveWidth-1-i) / float64(waveWidth-1-mid)
		}
		percent := uint32(ratio * 90)
		lighten := func(channel uint32) uint32 {
			value := channel >> 8
			return value + (255-value)*percent/100
		}
		color := fmt.Sprintf("#%02X%02X%02X", lighten(r), lighten(g), lighten(b))
		wave[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(color))
	}
	runes := []rune(text)
	frames := make([]string, len(runes)+waveWidth+wavePause)
	for position := range frames {
		var frame strings.Builder
		for i, char := range runes {
			style := base
			if distance := position - i; distance >= 0 && distance < waveWidth {
				style = wave[distance]
			}
			frame.WriteString(style.Render(string(char)))
		}
		frames[position] = frame.String()
	}
	return frames
}

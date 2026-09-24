// Package ui contains terminal-only presentation helpers.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

const progressTitle = "Calculating recommendations"

// Progress renders a spinner and shimmering title without switching terminal
// modes or sending terminal capability queries.
type Progress struct {
	out      io.Writer
	total    int
	done     chan struct{}
	finished chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	current  int
	lineSize int
}

// StartProgress starts a progress renderer when enabled.
func StartProgress(total int, enabled, noColor bool) *Progress {
	return startProgress(total, enabled, &colorprofile.Writer{Forward: os.Stderr, Profile: ColorProfile(os.Stderr, noColor)})
}

func startProgress(total int, enabled bool, out io.Writer) *Progress {
	if !enabled {
		return &Progress{}
	}
	p := &Progress{out: out, total: total, done: make(chan struct{}), finished: make(chan struct{})}
	go p.run()
	return p
}

func (p *Progress) run() {
	defer close(p.finished)
	frames := shimmerFrames(progressTitle)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	spinnerTicker := time.NewTicker(spinner.Dot.FPS)
	defer spinnerTicker.Stop()
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(progressColor))
	frame, spinnerFrame := 0, 0
	render := func() {
		p.render(style.Render(spinner.Dot.Frames[spinnerFrame]) + frames[frame])
	}
	render()
	for {
		select {
		case <-ticker.C:
			frame = (frame + 1) % len(frames)
		case <-spinnerTicker.C:
			spinnerFrame = (spinnerFrame + 1) % len(spinner.Dot.Frames)
		case <-p.done:
			p.clear()
			return
		}
		render()
	}
}

func (p *Progress) render(frame string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := fmt.Sprintf("%s %d/%d", frame, p.current, p.total)
	p.lineSize = lipgloss.Width(line)
	_, _ = fmt.Fprintf(p.out, "\r%s", line)
}

func (p *Progress) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.out, "\r%s\r", strings.Repeat(" ", p.lineSize))
}

// Increment advances the displayed count by one.
func (p *Progress) Increment() {
	if p.done == nil {
		return
	}
	p.mu.Lock()
	p.current++
	p.mu.Unlock()
}

// Stop clears the progress line and stops its renderer. It is safe to call
// more than once.
func (p *Progress) Stop() {
	if p.done == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.done) })
	<-p.finished
}

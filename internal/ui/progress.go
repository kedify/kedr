// Package ui contains terminal-only presentation helpers.
package ui

import (
	"fmt"
	"os"
	"sync"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type progressMsg int
type doneMsg struct{}

type progressModel struct {
	spinner spinner.Model
	current int
	total   int
}

func newProgressModel(total int) *progressModel {
	s := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("#00afaf"))))
	return &progressModel{spinner: s, total: total}
}

func (m *progressModel) Init() tea.Cmd { return m.spinner.Tick }
func (m *progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := msg.(type) {
	case progressMsg:
		m.current = int(typed)
		return m, nil
	case doneMsg:
		return m, tea.Quit
	default:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
}
func (m *progressModel) View() tea.View {
	return tea.NewView(fmt.Sprintf("%s Calculating recommendations %d/%d", m.spinner.View(), m.current, m.total))
}

type Progress struct {
	program *tea.Program
	done    chan struct{}
	mu      sync.Mutex
	current int
}

func StartProgress(total int, enabled bool) *Progress {
	if !enabled {
		return &Progress{}
	}
	p := tea.NewProgram(newProgressModel(total), tea.WithInput(nil), tea.WithOutput(os.Stderr))
	progress := &Progress{program: p, done: make(chan struct{})}
	go func() { _, _ = p.Run(); close(progress.done) }()
	return progress
}

func (p *Progress) Increment() {
	if p.program == nil {
		return
	}
	p.mu.Lock()
	p.current++
	current := p.current
	p.mu.Unlock()
	p.program.Send(progressMsg(current))
}

func (p *Progress) Stop() {
	if p.program == nil {
		return
	}
	p.program.Send(doneMsg{})
	<-p.done
}

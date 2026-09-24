package report

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

func TestTableDiffColors(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	const mi = 1024 * 1024
	r := fixture()
	r.Scans = nil
	for _, row := range []struct {
		name           string
		cpu, memory    float64
		pods           int
		unset          bool
		unknownCurrent bool
		oom            bool
	}{
		{name: "small-save", cpu: -.01, memory: -500 * mi, pods: 1},
		{name: "medium-save", cpu: -.5, memory: -300 * mi, pods: 1},
		{name: "largest-save", cpu: -.4, memory: -10 * mi, pods: 5},
		{name: "small-increase", cpu: .01, memory: 500 * mi, pods: 1},
		{name: "medium-increase", cpu: .5, memory: 300 * mi, pods: 1},
		{name: "largest-increase", cpu: .4, memory: 10 * mi, pods: 5},
		{name: "unchanged", pods: 1},
		{name: "rounded-unchanged", cpu: .0000001, memory: 1, pods: 1},
		{name: "zero-pods", cpu: 100, memory: 10000 * mi},
		{name: "oom-zero-pods", cpu: 100, memory: 50 * mi, oom: true},
		{name: "unset", cpu: 100, memory: 10000 * mi, pods: 100, unset: true},
		{name: "unknown", cpu: 100, memory: 10000 * mi, pods: 100, unknownCurrent: true},
	} {
		scan := fixture().Scans[0]
		scan.Object.Name = row.name
		scan.Object.Pods = make([]model.Pod, row.pods)
		if row.oom {
			scan.Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{Resource: analysis.ResourceMemory, Evidence: analysis.ResourceEvidence{OOMKills: []analysis.OOMKill{{Timestamp: 1}}}}}}
		}
		for _, entry := range []struct {
			resource model.ResourceType
			current  float64
			delta    float64
		}{{model.CPU, 2, row.cpu}, {model.Memory, 1024 * mi, row.memory}} {
			current := model.Number(entry.current)
			if row.unset {
				current = model.Unset()
			} else if row.unknownCurrent {
				current = model.Unknown()
			}
			scan.Object.Allocations.Requests[entry.resource] = current
			scan.Recommended.Requests[entry.resource] = model.RecommendationValue{Value: model.Number(entry.current + entry.delta)}
		}
		r.Scans = append(r.Scans, scan)
	}
	for _, cluster := range []bool{false, true} {
		cfg := config.Default("simple")
		cfg.Full, cfg.ShowClusterName = true, cluster
		colored, err := Render(r, cfg, true)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := Render(r, cfg, false)
		if err != nil || ansi.Strip(colored) != plain {
			t.Fatal("colors changed table values, alignment or row order")
		}
		cells := diffCellColors(t, colored)
		for _, tc := range []struct{ name, cpu, memory string }{
			{"small-save", "135;149;140", "57;255;136"},
			{"largest-save", "57;255;136", "135;149;140"},
			{"small-increase", "163;152;144", "255;59;78"},
			{"largest-increase", "255;59;78", "163;152;144"},
			{"unchanged", "", ""},
			{"rounded-unchanged", "", ""},
			{"zero-pods", "146;146;146", "146;146;146"},
			{"oom-zero-pods", "146;146;146", "163;152;144"},
			{"unset", "", ""},
			{"unknown", "146;146;146", "146;146;146"},
		} {
			if got := cells[tc.name]; got != [2]string{tc.cpu, tc.memory} {
				t.Fatalf("%s colors = %v, want %s / %s", tc.name, got, tc.cpu, tc.memory)
			}
		}
		for _, group := range [][3]string{{"small-save", "medium-save", "largest-save"}, {"small-increase", "medium-increase", "largest-increase"}} {
			for resource := range 2 {
				if cells[group[1]][resource] == cells[group[0]][resource] || cells[group[1]][resource] == cells[group[2]][resource] {
					t.Fatalf("middle change did not get an intermediate shade: %v", cells)
				}
			}
		}
	}
}

func diffCellColors(t *testing.T, text string) map[string][2]string {
	t.Helper()
	result := map[string][2]string{}
	columns := map[string]int{}
	foreground := regexp.MustCompile(`\x1b\[[0-9;]*38;2;(\d+;\d+;\d+)m`)
	for _, line := range strings.Split(text, "\n") {
		cells := strings.Split(line, "│")
		if len(cells) < 3 {
			continue
		}
		if strings.TrimSpace(ansi.Strip(cells[1])) == "Number" {
			for i, cell := range cells {
				columns[strings.TrimSpace(ansi.Strip(cell))] = i
			}
			continue
		}
		var colors [2]string
		for i, column := range []string{"CPU Diff", "Memory Diff"} {
			if strings.TrimSpace(ansi.Strip(cells[columns[column]])) == "" {
				continue
			}
			match := foreground.FindStringSubmatch(cells[columns[column]])
			if len(match) != 2 {
				t.Fatalf("missing diff color in %q", cells[columns[column]])
			}
			colors[i] = match[1]
		}
		result[strings.TrimSpace(ansi.Strip(cells[columns["Name"]]))] = colors
	}
	return result
}

func TestTableColorOptOut(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	for _, noColor := range []string{"", "1", "0", "false", "any-value"} {
		t.Setenv("NO_COLOR", noColor)
		for _, flag := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				cfg := config.Default("simple")
				cfg.NoColor = flag
				text, err := Render(fixture(), cfg, terminal)
				if err != nil {
					t.Fatal(err)
				}
				wantColor := terminal && !flag && noColor == ""
				if strings.ContainsRune(text, '\x1b') != wantColor {
					t.Fatalf("terminal=%t no-color=%t NO_COLOR=%q: unexpected ANSI output", terminal, flag, noColor)
				}
			}
		}
	}
}

func TestTableSingleAndEqualDiffs(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	r := fixture()
	for count := 1; count <= 2; count++ {
		text, err := Render(r, config.Default("simple"), true)
		if err != nil {
			t.Fatal(err)
		}
		for name, colors := range diffCellColors(t, text) {
			if colors != [2]string{"57;255;136", "57;255;136"} {
				t.Fatalf("%s: equal maximum savings should share the brightest green: %v", name, colors)
			}
		}
		scan := fixture().Scans[0]
		scan.Object.Name = "same-saving"
		// Both CPU deltas display -44m despite different floating-point residues.
		scan.Object.Allocations.Requests[model.CPU] = model.Number(.1)
		scan.Recommended.Requests[model.CPU] = model.RecommendationValue{Value: model.Number(.056)}
		r.Scans = append(r.Scans, scan)
	}
}

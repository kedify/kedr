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

func TestPotentialMemoryLeakTableColumn(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	report := fixture()
	leak := &report.Scans[0]
	for _, kind := range model.ResourceTypes {
		leak.Recommended.Requests[kind] = model.RecommendationValue{Value: leak.Object.Allocations.Requests[kind]}
		leak.Recommended.Limits[kind] = model.RecommendationValue{Value: leak.Object.Allocations.Limits[kind]}
	}
	leak.Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{
		Resource:   analysis.ResourceMemory,
		MemoryLeak: &analysis.MemoryLeakAnalysis{Status: analysis.MemoryLeakPotential},
	}}}

	clear := fixture().Scans[0]
	clear.Object.Name = "clear"
	clear.Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{
		Resource:   analysis.ResourceMemory,
		MemoryLeak: &analysis.MemoryLeakAnalysis{Status: analysis.MemoryLeakNoPattern},
	}}}
	report.Scans = append(report.Scans, clear)

	cfg := config.Default("simple")
	cfg.DetectMemoryLeaks, cfg.UseOOMKillData = true, true
	plain, err := Render(report, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain, "Potential Mem Leak") || !strings.Contains(plain, "OOMKilled") {
		t.Fatalf("missing opted-in columns: %s", plain)
	}
	if row := firstTableRow(t, plain); row[len(row)-1] != "yes" || row[len(row)-2] != "" {
		t.Fatalf("unchanged leak row or OOM cell: %v", row)
	}
	clearFound := false
	for _, line := range strings.Split(plain, "\n") {
		cells := strings.Split(line, "│")
		if len(cells) < 3 || strings.TrimSpace(cells[1]) != "2." {
			continue
		}
		clearFound = true
		if strings.TrimSpace(cells[len(cells)-2]) != "" {
			t.Fatalf("non-positive leak row marked positive: %s", line)
		}
	}
	if !clearFound {
		t.Fatalf("non-positive leak row missing: %s", plain)
	}

	colored, err := Render(report, cfg, true)
	if err != nil || ansi.Strip(colored) != plain {
		t.Fatalf("color changed the table content: %v", err)
	}
	red := regexp.MustCompile(`\x1b\[[0-9;]*38;2;255;59;78m`)
	for _, line := range strings.Split(colored, "\n") {
		cells := strings.Split(line, "│")
		if len(cells) < 3 {
			continue
		}
		cell := cells[len(cells)-2]
		if strings.TrimSpace(ansi.Strip(cell)) == "yes" && !red.MatchString(cell) {
			t.Fatalf("positive leak cell is not red: %q", cell)
		}
	}

	cfg.DetectMemoryLeaks = false
	withoutFlag, err := Render(report, cfg, false)
	if err != nil || strings.Contains(withoutFlag, "Potential Mem Leak") || strings.Contains(withoutFlag, "yes") || strings.Contains(withoutFlag, "│1.") {
		t.Fatalf("leak finding appeared without the flag: %s, %v", withoutFlag, err)
	}
}

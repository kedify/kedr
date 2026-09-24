package report

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

func TestOOMTimeDisplay(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 24, 16, 30, 0, 0, zone)
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{0, "Yes (0s ago)"},
		{59 * time.Second, "Yes (59s ago)"},
		{time.Minute, "Yes (1m ago)"},
		{5 * time.Minute, "Yes (5m ago)"},
		{15 * time.Minute, "Yes (15m ago)"},
		{15*time.Minute + time.Second, "Yes (16:14:59)"},
		{24 * time.Hour, "Yes (16:30:00 yesterday)"},
		{48 * time.Hour, "Yes (16:30:00 2026-09-22)"},
	} {
		if got := oomCell(now.Add(-tc.age).UnixMilli(), now); got != tc.want {
			t.Errorf("age %s: got %q, want %q", tc.age, got, tc.want)
		}
	}
	if oomCell(0, now) != "" {
		t.Fatal("no event must leave the cell blank")
	}
	// Yesterday is a calendar day, including the 23-hour daylight-saving day.
	now = time.Date(2026, time.March, 30, 0, 5, 0, 0, zone)
	event := time.Date(2026, time.March, 29, 0, 10, 0, 0, zone)
	if got := oomCell(event.UnixMilli(), now); got != "Yes (00:10:00 yesterday)" {
		t.Fatal(got)
	}
}

func TestOOMTableColumn(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	r := fixture()
	scan := &r.Scans[0]
	for _, kind := range model.ResourceTypes {
		scan.Recommended.Requests[kind] = model.RecommendationValue{Value: scan.Object.Allocations.Requests[kind]}
		scan.Recommended.Limits[kind] = model.RecommendationValue{Value: scan.Object.Allocations.Limits[kind]}
	}
	latest := time.Now().Add(-5 * time.Minute).UnixMilli()
	scan.Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{Resource: analysis.ResourceMemory, Evidence: analysis.ResourceEvidence{OOMKills: []analysis.OOMKill{{Timestamp: latest}, {Timestamp: latest - 60000}}}}}}
	// Raw adapter events may include other rollouts. Only selected evidence is displayed.
	scan.Object.OOMKills = []analysis.OOMKill{{Timestamp: latest + 60000}}
	if latestOOM(*scan) != latest {
		t.Fatal("column selected an unfiltered event or missed the latest event")
	}
	r.Scans = append(r.Scans, fixture().Scans[0])
	for _, cluster := range []bool{false, true} {
		for _, color := range []bool{false, true} {
			cfg := config.Default("simple")
			cfg.UseOOMKillData, cfg.ShowClusterName = true, cluster
			text, err := Render(r, cfg, color)
			if err != nil {
				t.Fatal(err)
			}
			plain := ansi.Strip(text)
			if row := firstTableRow(t, plain); row[len(row)-1] != "Yes (5m ago)" {
				t.Fatalf("OOM column must be last, including otherwise unchanged rows: %v", row)
			}
			for _, line := range strings.Split(plain, "\n") {
				cells := strings.Split(line, "│")
				if len(cells) < 3 {
					continue
				}
				if strings.TrimSpace(cells[1]) == "Number" && strings.TrimSpace(cells[len(cells)-2]) != "OOMKilled" {
					t.Fatal("OOM header must be last")
				}
				if strings.TrimSpace(cells[1]) == "2." && strings.TrimSpace(cells[len(cells)-2]) != "" {
					t.Fatal("row without an OOM must have an empty OOM cell")
				}
			}
			cfg.UseOOMKillData = false
			text, err = Render(r, cfg, color)
			if err != nil || strings.Contains(text, "OOMKilled") || strings.Contains(text, "Yes (") {
				t.Fatalf("OOM column appeared without opt-in: %s, %v", text, err)
			}
		}
	}
}

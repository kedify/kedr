package report

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

// Compare the rendered DOM with the original CSV contract, including optional
// columns, note boundaries, and untrusted strings that must remain plain text.
func TestHTMLPreservesContentAndEscapesMarkup(t *testing.T) {
	r := fixture()
	attack := `<script>alert("<&>")</script><img src=x onerror=alert(1)>`
	r.Scans[0].Object.Name = attack
	r.Scans[0].Object.Warnings = []string{"first; second <warning>", attack}
	r.Scans[0].Recommended.Info[model.Memory] = &attack
	r.Scans[0].ReleaseComparisons = []model.ReleaseComparison{
		{Release: model.Release{ID: "current", Name: attack, Image: attack, Current: true}},
		{Release: model.Release{ID: "previous", Image: "registry.example/app:v1@sha256:abcdef"}, SizingResources: []model.ResourceType{model.Memory}},
	}
	r.Scans[0].Analysis = &analysis.Output{}
	for _, severity := range []model.Severity{model.SeverityGood, model.SeverityOK, model.SeverityWarning, model.SeverityUnknown, model.Severity(attack)} {
		scan := r.Scans[0]
		scan.Object.Cluster = nil
		scan.Severity = severity
		r.Scans = append(r.Scans, scan)
	}
	for _, cluster := range []bool{false, true} {
		for _, severity := range []bool{false, true} {
			t.Run(fmt.Sprintf("cluster=%t/severity=%t", cluster, severity), func(t *testing.T) {
				cfg := config.Default("simple")
				cfg.Format, cfg.ShowClusterName, cfg.ShowSeverity = "html", cluster, severity
				output, err := Render(r, cfg, false)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(output, attack) || strings.Contains(output, "ZgotmplZ") {
					t.Fatal("unsafe or broken HTML template")
				}
				doc, err := html.Parse(strings.NewReader(output))
				if err != nil {
					t.Fatal(err)
				}
				for _, tag := range []string{"script", "img"} {
					if len(htmlElements(doc, tag)) != 0 {
						t.Fatalf("untrusted content created a %s element", tag)
					}
				}
				csvOutput, err := renderCSV(r, cfg, false)
				if err != nil {
					t.Fatal(err)
				}
				want, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				rows := htmlElements(doc, "tr")
				if len(rows) != len(want) {
					t.Fatalf("row count = %d, want %d", len(rows), len(want))
				}
				for i, row := range rows {
					tag := "td"
					if i == 0 {
						tag = "th"
					}
					cells := htmlElements(row, tag)
					if len(cells) != len(want[i]) {
						t.Fatalf("row %d column count = %d, want %d", i, len(cells), len(want[i]))
					}
					for j, cell := range cells {
						got := strings.TrimSpace(htmlText(cell))
						if notes := htmlElements(cell, "li"); len(notes) > 0 {
							parts := make([]string, len(notes))
							for k, note := range notes {
								parts[k] = htmlText(note)
							}
							got = strings.Join(parts, "; ")
						}
						if got != want[i][j] {
							t.Errorf("cell [%d,%d] = %q, want %q", i, j, got, want[i][j])
						}
					}
				}
				images := 0
				for _, code := range htmlElements(doc, "code") {
					if htmlText(code) == attack {
						for _, attr := range code.Attr {
							if attr.Key == "class" && attr.Val == "image" {
								images++
							}
						}
					}
				}
				if images != len(r.Scans) {
					t.Fatalf("code-styled image references = %d, want %d", images, len(r.Scans))
				}
			})
		}
	}
}

func htmlElements(n *html.Node, tag string) []*html.Node {
	var nodes []*html.Node
	if n.Type == html.ElementNode && n.Data == tag {
		nodes = append(nodes, n)
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		nodes = append(nodes, htmlElements(child, tag)...)
	}
	return nodes
}

func htmlText(n *html.Node) string {
	for _, attr := range n.Attr {
		if attr.Key == "aria-hidden" && attr.Val == "true" {
			return "" // Decorative emoji are additional to the original severity text.
		}
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(htmlText(child))
	}
	return b.String()
}

// Opt-in artifacts support visual inspection without committing generated output.
func TestHTMLVisualFixtures(t *testing.T) {
	dir := os.Getenv("KEDR_VISUAL_DIR")
	if dir == "" {
		t.Skip("set KEDR_VISUAL_DIR to export review fixtures")
	}
	r := fixture()
	scan := r.Scans[0]
	r.Scans = nil
	for i, severity := range []model.Severity{model.SeverityCritical, model.SeverityWarning, model.SeverityOK, model.SeverityGood, model.SeverityUnknown} {
		scan.Severity = severity
		scan.Object.Name = []string{"checkout-api", "payment-worker", "inventory", "frontend", "new-service"}[i]
		scan.Object.Container = "app"
		scan.ReleaseComparisons = nil
		scan.Object.Warnings = nil
		if i == 0 {
			scan.Object.Warnings = []string{"Historical ownership was unavailable for some pods; their samples were excluded."}
			scan.ReleaseComparisons = []model.ReleaseComparison{
				{Release: model.Release{ID: "current", Name: "checkout-api-7b9df8", Image: "registry.example.com/production/checkout-api:v2.4.1@sha256:" + strings.Repeat("abcdef0123456789", 4), Current: true}, SizingResources: []model.ResourceType{model.CPU, model.Memory}, CPU: model.ReleaseUsage{AggregatedUsage: analysis.Signal{Available: true, Value: 120}}, Memory: model.ReleaseUsage{AggregatedUsage: analysis.Signal{Available: true, Value: 256 * 1024 * 1024}, HistoryHours: 192}},
				{Release: model.Release{ID: "previous", Image: "registry.example.com/production/checkout-api:v2.3.0"}, Memory: model.ReleaseUsage{HistoryHours: 24}},
			}
		}
		r.Scans = append(r.Scans, scan)
	}
	r.CalculateScore()
	cfg := config.Default("simple")
	cfg.Format = "html"
	for _, name := range []string{"report", "report-empty"} {
		if name == "report-empty" {
			r.Scans = nil
			r.CalculateScore()
		}
		output, err := Render(r, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		// #nosec G703 -- Explicit opt-in export directory; filenames are fixed fixture names.
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(output), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

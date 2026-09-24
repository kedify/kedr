package report

import (
	_ "embed"
	"html/template"
	"strings"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/resource"
)

//go:embed report.html
var htmlPage string

var htmlTemplate = template.Must(template.New("report").Parse(htmlPage))

type htmlRow struct {
	Object   model.Object
	Severity htmlSeverity
	Values   []string
	Notes    []scanNote
}

type htmlSeverity struct {
	Label model.Severity
	Class string
	Emoji string
}

func severityBadge(severity model.Severity) htmlSeverity {
	badge := htmlSeverity{Label: severity, Class: "unknown", Emoji: "❔"}
	switch severity {
	case model.SeverityUnknown:
		// Keep the default unknown badge.
	case model.SeverityGood:
		badge.Class, badge.Emoji = "good", "✅"
	case model.SeverityOK:
		badge.Class, badge.Emoji = "ok", "🟢"
	case model.SeverityWarning:
		badge.Class, badge.Emoji = "warning", "⚠️"
	case model.SeverityCritical:
		badge.Class, badge.Emoji = "critical", "🔴"
	}
	return badge
}

func renderHTML(report model.Report, cfg *config.Config) (string, error) {
	rows := make([]htmlRow, 0, len(report.Scans))
	for _, scan := range report.Scans {
		row := htmlRow{
			Object:   scan.Object,
			Severity: severityBadge(scan.Severity),
			Notes:    scanNoteEntries(scan, legacyQuantity),
		}
		for _, r := range model.ResourceTypes {
			row.Values = append(row.Values,
				diff(scan.Object.Allocations.Requests[r], scan.Recommended.Requests[r], scan.Object.CurrentPods(), resource.Format),
				transition(scan.Object.Allocations.Requests[r], scan.Recommended.Requests[r], true, resource.Format),
				transition(scan.Object.Allocations.Limits[r], scan.Recommended.Limits[r], false, resource.Format),
			)
		}
		rows = append(rows, row)
	}
	data := struct {
		Headers      []string
		Rows         []htmlRow
		ShowCluster  bool
		ShowSeverity bool
		Score        int
		ScoreLetter  string
	}{headers(cfg, false), rows, cfg.ShowClusterName, cfg.ShowSeverity, report.Score, report.ScoreLetter()}
	var b strings.Builder
	err := htmlTemplate.Execute(&b, data)
	return b.String(), err
}

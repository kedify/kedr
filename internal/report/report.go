// Package report renders recommendation reports.
package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	liptable "charm.land/lipgloss/v2/table"
	"gopkg.in/yaml.v3"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/resource"
)

func Render(value model.Report, cfg *config.Config, color bool) (string, error) {
	switch cfg.Format {
	case "json":
		data, err := json.MarshalIndent(value, "", "  ")
		return string(data), err
	case "yaml":
		data, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		var node yaml.Node
		if err = yaml.Unmarshal(data, &node); err != nil {
			return "", err
		}
		output, err := yaml.Marshal(&node)
		return string(output), err
	case "csv":
		return renderCSV(value, cfg, false)
	case "csv-raw":
		return renderCSV(value, cfg, true)
	case "pprint":
		return pprint(value)
	case "html":
		return renderHTML(value, cfg)
	default:
		return renderTable(value, cfg, color && !cfg.NoColor && os.Getenv("NO_COLOR") == ""), nil
	}
}

func value(v model.MaybeValue, format func(float64) string) string {
	if v.Unknown {
		return "?"
	}
	if !v.Set {
		return "unset"
	}
	return format(v.Value)
}
func rawValue(v model.MaybeValue) string {
	if v.Unknown {
		return "?"
	}
	if !v.Set {
		return "unset"
	}
	return strconv.FormatFloat(v.Value, 'f', -1, 64)
}
func diff(current model.MaybeValue, recommended model.RecommendationValue, multiplier int, format func(float64) string) string {
	if !recommended.Value.Set || recommended.Value.Unknown {
		return ""
	}
	currentValue := 0.0
	if current.Set && !current.Unknown {
		currentValue = current.Value
	}
	delta := (recommended.Value.Value - currentValue) * float64(multiplier)
	if delta < 0 {
		if text := format(-delta); text != format(0) {
			return "-" + text
		}
		return "+" + format(0) // Do not show negative zero after display rounding.
	}
	return "+" + format(delta)
}
func transition(current model.MaybeValue, recommended model.RecommendationValue, requests bool, format func(float64) string) string {
	d := ""
	if requests {
		d = diff(current, recommended, 1, format)
		if d != "" {
			d = "(" + d + ") "
		}
	}
	return d + value(current, format) + " -> " + value(recommended.Value, format)
}

func tableQuantity(kind model.ResourceType) func(float64) string {
	if kind == model.CPU {
		return resource.FormatCPU
	}
	return resource.FormatMemory
}

func tableTransition(current model.MaybeValue, recommended model.RecommendationValue, format func(float64) string) string {
	// Compare displayed quantities, but retain unknown recommendations as evidence gaps.
	if !recommended.Value.Unknown && value(current, format) == value(recommended.Value, format) {
		return ""
	}
	return transition(current, recommended, false, format)
}

func legacyQuantity(model.ResourceType) func(float64) string { return resource.Format }

func headers(cfg *config.Config, raw bool) []string {
	h := []string{"Namespace", "Name", "Pods", "Old Pods", "Type", "Container"}
	if cfg.ShowClusterName {
		h = append([]string{"Cluster"}, h...)
	}
	if cfg.ShowSeverity {
		h = append(h, "Severity")
	}
	for _, r := range []string{"CPU", "Memory"} {
		if raw {
			h = append(h, r+" Requests Current", r+" Requests Recommended", r+" Limits Current", r+" Limits Recommended")
		} else {
			h = append(h, r+" Diff", r+" Requests", r+" Limits")
		}
	}
	return append(h, "Notes")
}
func renderCSV(report model.Report, cfg *config.Config, raw bool) (string, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.Write(headers(cfg, raw)); err != nil {
		return "", err
	}
	for _, scan := range report.Scans {
		row := []string{scan.Object.Namespace, scan.Object.Name, strconv.Itoa(scan.Object.CurrentPods()), strconv.Itoa(scan.Object.DeletedPods()), scan.Object.Kind, scan.Object.Container}
		if cfg.ShowClusterName {
			name := ""
			if scan.Object.Cluster != nil {
				name = *scan.Object.Cluster
			}
			row = append([]string{name}, row...)
		}
		if cfg.ShowSeverity {
			row = append(row, string(scan.Severity))
		}
		for _, r := range model.ResourceTypes {
			if raw {
				row = append(row, rawValue(scan.Object.Allocations.Requests[r]), rawValue(scan.Recommended.Requests[r].Value), rawValue(scan.Object.Allocations.Limits[r]), rawValue(scan.Recommended.Limits[r].Value))
			} else {
				row = append(row, diff(scan.Object.Allocations.Requests[r], scan.Recommended.Requests[r], scan.Object.CurrentPods(), resource.Format), transition(scan.Object.Allocations.Requests[r], scan.Recommended.Requests[r], true, resource.Format), transition(scan.Object.Allocations.Limits[r], scan.Recommended.Limits[r], false, resource.Format))
			}
		}
		row = append(row, scanNotes(scan, legacyQuantity))
		if err := w.Write(row); err != nil {
			return "", err
		}
	}
	w.Flush()
	return b.String(), w.Error()
}

func renderTable(report model.Report, cfg *config.Config, color bool) string {
	h := []string{"Number"}
	clusters := map[string]bool{}
	visible := make([]int, 0, len(report.Scans))
	for i, s := range report.Scans {
		if !cfg.Full && !hasTableChange(s) {
			continue
		}
		visible = append(visible, i)
		if s.Object.Cluster != nil {
			clusters[*s.Object.Cluster] = true
		}
	}
	showCluster := cfg.ShowClusterName || len(clusters) > 1
	if showCluster {
		h = append(h, "Cluster")
	}
	h = append(h, "Namespace", "Name", "Kind", "Pods", "Old Pods", "Container")
	firstResourceColumn := len(h)
	for _, r := range []string{"CPU", "Memory"} {
		h = append(h, r+" Diff", r+" Requests", r+" Limits")
	}
	lastResourceColumn := len(h)
	rows := make([][]string, 0, len(visible))
	for _, i := range visible {
		scan := report.Scans[i]
		// Keep the saved-run row number so kedr explain selects the same scan.
		row := []string{fmt.Sprintf("%d.", i+1)}
		if showCluster {
			name := ""
			if scan.Object.Cluster != nil {
				name = *scan.Object.Cluster
			}
			row = append(row, name)
		}
		row = append(row, scan.Object.Namespace, scan.Object.Name, scan.Object.Kind, strconv.Itoa(scan.Object.CurrentPods()), strconv.Itoa(scan.Object.DeletedPods()), scan.Object.Container)
		for _, r := range model.ResourceTypes {
			format := tableQuantity(r)
			current := scan.Object.Allocations.Requests[r]
			recommended := scan.Recommended.Requests[r]
			request := tableTransition(current, recommended, format)
			change := diff(current, recommended, scan.Object.CurrentPods(), format)
			if request == "" || (!current.Set && !current.Unknown && recommended.Value.Set && !recommended.Value.Unknown) {
				change = ""
			}
			row = append(row, change, request, tableTransition(scan.Object.Allocations.Limits[r], scan.Recommended.Limits[r], format))
		}
		rows = append(rows, row)
	}
	for col := firstResourceColumn; col < lastResourceColumn; col += 3 {
		alignTableArrows(rows, col+1)
		alignTableArrows(rows, col+2)
	}
	t := liptable.New().Headers(h...).Rows(rows...).Border(lipgloss.RoundedBorder())
	if cfg.Width != nil {
		t = t.Width(*cfg.Width)
	}
	if color {
		diffColors := []diffColorScale{
			newDiffColorScale(report.Scans, visible, model.CPU),
			newDiffColorScale(report.Scans, visible, model.Memory),
		}
		t = t.StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle()
			if row == liptable.HeaderRow {
				return style.Bold(true).Foreground(lipgloss.Color("#d75fd7"))
			}
			if col > 0 && col < firstResourceColumn {
				return style.Foreground(lipgloss.Color("#00afaf"))
			}
			if row >= 0 && row < len(visible) && col >= firstResourceColumn && col < lastResourceColumn && (col-firstResourceColumn)%3 == 0 {
				return diffColors[(col-firstResourceColumn)/3].style(row)
			}
			return style
		})
	}
	title := stripMarkup(report.Description)
	if title != "" {
		title += "\n\n"
	}
	table := title + t.String()
	if len(visible) == 0 && len(report.Scans) > 0 {
		table = title + "No resource changes to display. Use --full to show all rows."
	}
	if !cfg.Explain {
		return table
	}
	var notes strings.Builder
	for _, i := range visible {
		scan := report.Scans[i]
		if text := scanNotes(scan, tableQuantity); text != "" {
			fmt.Fprintf(&notes, "\n%s/%s/%s: %s", scan.Object.Namespace, scan.Object.Name, scan.Object.Container, text)
		}
	}
	return table + "\nDiff columns: total request change across current pods. Requests and limits are per container. Values are rounded for display." + notes.String() + fmt.Sprintf("\n%d points - %s", report.Score, report.ScoreLetter())
}

func hasTableChange(scan model.Scan) bool {
	for _, r := range model.ResourceTypes {
		format := tableQuantity(r)
		for _, setting := range []struct{ current, recommended model.MaybeValue }{
			{scan.Object.Allocations.Requests[r], scan.Recommended.Requests[r].Value},
			{scan.Object.Allocations.Limits[r], scan.Recommended.Limits[r].Value},
		} {
			// Compare displayed values: a rounded X -> X is also a table no-op.
			if !setting.recommended.Unknown && value(setting.current, format) != value(setting.recommended, format) {
				return true
			}
		}
	}
	return false
}

func alignTableArrows(rows [][]string, col int) {
	width := 0
	for _, row := range rows {
		width = max(width, strings.Index(row[col], " -> "))
	}
	for _, row := range rows {
		if index := strings.Index(row[col], " -> "); index >= 0 {
			row[col] = strings.Repeat(" ", width-index) + row[col]
		}
	}
}

// scanNote retains image references separately for rich output. String preserves
// the plain-text representation used by CSV and terminal reports.
type scanNote struct {
	Text   string
	Image  string
	Detail string
}

func (n scanNote) String() string {
	text := n.Text
	if n.Image != "" {
		text += " (" + n.Image + ")"
	}
	if n.Detail != "" {
		text += " [" + n.Detail + "]"
	}
	return text
}

func scanNotes(scan model.Scan, format func(model.ResourceType) func(float64) string) string {
	notes := scanNoteEntries(scan, format)
	parts := make([]string, len(notes))
	for i, note := range notes {
		parts[i] = note.String()
	}
	return strings.Join(parts, "; ")
}

func scanNoteEntries(scan model.Scan, format func(model.ResourceType) func(float64) string) []scanNote {
	parts := make([]scanNote, 0, len(scan.Object.Warnings)+len(scan.ReleaseComparisons))
	for _, warning := range scan.Object.Warnings {
		parts = append(parts, scanNote{Text: warning})
	}
	hasSizingSources := false
	for _, comparison := range scan.ReleaseComparisons {
		hasSizingSources = hasSizingSources || len(comparison.SizingResources) > 0
	}
	for _, comparison := range scan.ReleaseComparisons {
		role := "comparison only"
		if comparison.Release.Current {
			role = "current release"
			// Older reports have no explicit source mapping.
			if !hasSizingSources && scan.Analysis == nil {
				role = "sizing release"
			}
		}
		if len(comparison.SizingResources) > 0 {
			resources := make([]string, 0, len(comparison.SizingResources))
			for _, resource := range comparison.SizingResources {
				resources = append(resources, string(resource))
			}
			role = "fallback sizing release for " + strings.Join(resources, ", ")
			if comparison.Release.Current {
				role = "current sizing release for " + strings.Join(resources, ", ")
			}
		}
		cpu, memory := "?", "?"
		if comparison.CPU.AggregatedUsage.Available {
			cpu = format(model.CPU)(comparison.CPU.AggregatedUsage.Value / 1000)
		}
		if comparison.Memory.AggregatedUsage.Available {
			memory = format(model.Memory)(comparison.Memory.AggregatedUsage.Value)
		}
		label := comparison.Release.Name
		if label == "" {
			label = comparison.Release.ID
		}
		parts = append(parts, scanNote{
			Text:   role + ": " + label,
			Image:  comparison.Release.Image,
			Detail: fmt.Sprintf("CPU aggregate %s; memory peak %s; memory history %.1fh", cpu, memory, comparison.Memory.HistoryHours),
		})
	}
	for _, resource := range model.ResourceTypes {
		if info := scan.Recommended.Info[resource]; info != nil && *info != "" {
			parts = append(parts, scanNote{Text: string(resource) + ": " + *info})
		}
	}
	return parts
}

func pprint(value model.Report) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var decoded any
	if err = json.Unmarshal(data, &decoded); err != nil {
		return "", err
	}
	return pythonRepr(decoded, 0), nil
}
func pythonRepr(value any, depth int) string {
	indent := strings.Repeat(" ", depth)
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return strconv.Quote(typed)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = pythonRepr(item, depth+1)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, indent+strconv.Quote(key)+": "+pythonRepr(typed[key], depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(typed)
	}
}
func stripMarkup(s string) string {
	r := strings.NewReplacer("[b]", "", "[/b]", "", "[underline]", "", "[/underline]", "")
	return r.Replace(s)
}

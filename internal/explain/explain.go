// Package explain turns immutable saved decisions into human-readable evidence.
package explain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	prom "github.com/kedify/kedr/internal/prometheus"
	"github.com/kedify/kedr/internal/resource"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

type Document struct {
	RunID       string
	Row         int
	ScanTime    string
	Version     string
	Strategy    string
	Target      string
	Cluster     string
	Window      string
	Start       int64
	End         int64
	Cards       []Card
	Resources   []Resource
	Releases    []Release
	Warnings    []string
	ChartStatus string
	RetrievedAt string
	Technical   string
	Queries     []string
	Charts      []Chart
	OOMEvents   []Event
}
type Card struct {
	Title       string
	Current     string
	Recommended string
	Delta       string
	Outcome     string
	Reason      string
	Source      string
	Steps       []Step
}
type Step struct {
	Rule    string
	Detail  string
	Outcome string
}
type Resource struct {
	Name      string
	Status    string
	Quality   string
	Inventory string
	Source    string
	Reasons   []string
}
type Release struct {
	ID       string
	Name     string
	Image    string
	Revision int64
	When     string
	Role     string
	CPU      string
	Memory   string
	Quality  string
}
type Chart struct {
	Resource string        `json:"resource"`
	Unit     string        `json:"unit"`
	Series   []ChartSeries `json:"series"`
	Lines    []Line        `json:"lines"`
	Events   []Event       `json:"events"`
}
type ChartSeries struct {
	ID      string            `json:"id"`
	Pod     string            `json:"pod"`
	Release string            `json:"release"`
	Sizing  bool              `json:"sizing"`
	Points  []analysis.Sample `json:"points"`
	Cadence float64           `json:"cadence"`
}
type Line struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Style string  `json:"style"`
}
type Event struct {
	Time  int64  `json:"time"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

func instant(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC")
}
func quantity(r model.ResourceType, v model.MaybeValue) string {
	if v.Unknown {
		return "unknown"
	}
	if !v.Set {
		return "unset"
	}
	if r == model.CPU {
		return resource.FormatCPU(v.Value)
	}
	return resource.FormatMemory(v.Value)
}
func native(r model.ResourceType, v float64) string {
	if r == model.CPU {
		return fmt.Sprintf("%.6g mCPU", v)
	}
	return fmt.Sprintf("%.6g MiB", v/(1024*1024))
}
func Build(run runstore.Run, row runstore.Row) Document {
	o := row.Scan.Object
	d := Document{RunID: run.ID, Row: row.Number, ScanTime: run.CreatedAt.UTC().Format(time.RFC3339), Version: run.KedrVersion, Strategy: run.Strategy, Target: fmt.Sprintf("%s / %s %s / %s", o.Namespace, o.Kind, o.Name, o.Container), Cluster: "in-cluster", Window: instant(row.WindowStart) + " — " + instant(row.EvaluationTime), Start: row.WindowStart, End: row.EvaluationTime, Warnings: append([]string(nil), o.Warnings...), ChartStatus: "Not fetched"}
	if row.Connection.Context != nil {
		d.Cluster = *row.Connection.Context
	}
	for _, r := range model.ResourceTypes {
		var a *analysis.ResourceAnalysis
		if row.Scan.Analysis != nil {
			for i := range row.Scan.Analysis.Results {
				if string(row.Scan.Analysis.Results[i].Resource) == string(r) {
					a = &row.Scan.Analysis.Results[i]
					break
				}
			}
		}
		for _, setting := range []analysis.Setting{analysis.SettingRequests, analysis.SettingLimits} {
			current, next := o.Allocations.Requests[r], row.Scan.Recommended.Requests[r].Value
			title := "Memory request"
			if r == model.CPU {
				title = "CPU request"
			}
			if setting == analysis.SettingLimits {
				current, next = o.Allocations.Limits[r], row.Scan.Recommended.Limits[r].Value
				title = strings.TrimSuffix(title, "request") + "limit"
			}
			card := Card{Title: title, Current: quantity(r, current), Recommended: quantity(r, next), Outcome: "retained", Reason: "The saved report retained this setting."}
			if next.Unknown {
				card.Outcome = "unavailable"
				card.Reason = "A recommendation was unavailable in the saved scan."
			}
			if current.Set && next.Set && !current.Unknown && !next.Unknown {
				delta := next.Value - current.Value
				scale := 1.0
				if r == model.CPU {
					scale = 1000
				}
				card.Delta = fmt.Sprintf("Change: %s per container", native(r, delta*scale))
				if current.Value > 0 {
					card.Delta += fmt.Sprintf(" (%+.1f%%)", 100*delta/current.Value)
				}
			}
			if a != nil && a.DecisionTrace != nil {
				card.Source = sourceText(a.DecisionTrace.Source)
				for _, t := range a.DecisionTrace.Settings {
					if t.Setting != setting {
						continue
					}
					card.Outcome = t.Disposition
					switch t.Disposition {
					case "recommended":
						card.Reason = "The usage-based candidate passed the evaluated safety and material-change guards."
						if setting == analysis.SettingRequests && a.Evidence.CurrentRequest.Unset || setting == analysis.SettingLimits && a.Evidence.CurrentLimit.Unset {
							card.Reason = "The usage-based candidate passed the evaluated safety guards. This setting was unset, so minimum-change thresholds do not apply."
						}
						if a.OOMAdjustment != nil {
							card.Reason = "A current-rollout OOM kill supplied a memory sizing floor. The candidate passed the evaluated setting guards."
							if a.DecisionTrace.Source.Method == "OOM kill" {
								card.Reason += " Usage history, sample count, coverage, and usage freshness were not required."
							}
							if a.OOMAdjustment.UsedCurrentFallback {
								card.Reason += " The limit at termination was unknown; current memory settings supplied the fallback baseline."
							}
						}
					case "disabled":
						card.Reason = "This strategy changes requests only; the existing limit is preserved."
					case "retained":
						card.Reason = "The existing setting was retained."
					case "unavailable":
						card.Reason = "The analyzer could not safely evaluate this setting."
					}
					for _, reason := range t.Reasons {
						card.Reason += " " + reasonText(reason)
					}
					for _, s := range t.Steps {
						step := Step{Rule: s.Rule}
						if s.Passed != nil {
							if *s.Passed {
								step.Outcome = "passed"
							} else {
								step.Outcome = "blocked"
							}
						}
						keys := make([]string, 0, len(s.Values))
						for key := range s.Values {
							keys = append(keys, key)
						}
						sort.Strings(keys)
						var parts []string
						for _, key := range keys {
							value := native(r, s.Values[key])
							if key == "coefficient" || key == "ratio" {
								value = fmt.Sprintf("%g×", s.Values[key])
							}
							if key == "relativeChange" || key == "minimumRelativeChange" {
								value = fmt.Sprintf("%.3g%%", s.Values[key]*100)
							}
							parts = append(parts, fmt.Sprintf("%s: %s", humanLabel(key), value))
						}
						step.Detail = strings.Join(parts, " · ")
						switch s.Rule {
						case "usage × headroom":
							step.Detail = fmt.Sprintf("%s observed usage × %g headroom = %s", native(r, s.Values["usage"]), s.Values["coefficient"], native(r, s.Values["candidate"]))
						case "request × limit ratio":
							step.Detail = fmt.Sprintf("%s request × %g = %s limit candidate", native(r, s.Values["request"]), s.Values["ratio"], native(r, s.Values["candidate"]))
						case "OOM floor":
							step.Detail = fmt.Sprintf("Larger of %s baseline and %s OOM floor → %s", native(r, s.Values["baseline"]), native(r, s.Values["floor"]), native(r, s.Values["candidate"]))
							if oomOnly(a) {
								step.Detail = fmt.Sprintf("No qualifying usage baseline was used. The OOM floor sets the request candidate to %s.", native(r, s.Values["candidate"]))
							}
						case "OOM sizing without usage history":
							step.Detail = "A current-rollout OOM authorizes a memory increase without qualifying usage samples."
						case "request bounds", "limit bounds":
							step.Detail = fmt.Sprintf("%s → %s; allowed range %s to %s", native(r, s.Values["before"]), native(r, s.Values["candidate"]), native(r, s.Values["minimum"]), native(r, s.Values["maximum"]))
							if s.Values["before"] < s.Values["minimum"] && s.Values["candidate"] == s.Values["minimum"] {
								step.Detail = fmt.Sprintf("%s is below the configured minimum of %s → %s.", native(r, s.Values["before"]), native(r, s.Values["minimum"]), native(r, s.Values["candidate"]))
							}
						}

						card.Steps = append(card.Steps, step)
					}
				}
			} else {
				card.Reason += " Per-setting decision trace was not recorded."
			}
			if a != nil && a.OOMAdjustment != nil {
				card.Steps = append([]Step{{Rule: "OOM increase", Detail: oomCalculation(a, row.Scan.Analysis.EffectivePolicy.Memory.OOMKilledCoefficient)}}, card.Steps...)
			}
			if why := row.Scan.SuppressedResources[r]; why != "" {
				card.Outcome = "suppressed"
				card.Reason = "Kedr suppressed this recommendation: " + why + ". Analyzer calculations below are audit evidence, not an actionable recommendation."
			}
			d.Cards = append(d.Cards, card)
		}
		if a != nil {
			q := a.DataQuality
			p := row.Scan.Analysis.EffectivePolicy.Evidence
			evidence := Resource{Name: string(r), Status: string(q.Status), Quality: fmt.Sprintf("History %.2fh / required %.2fh · observations %d / required %d · coverage %.1f%% / required %.1f%% · %d samples across %d series. Observed %s — %s. Freshness threshold %ds.", q.ObservedIntervalHours, float64(p.MinimumHistorySeconds)/3600, q.ObservationCount, p.MinimumSamples, 100*q.Coverage, 100*p.MinimumCoverage, q.SampleCount, q.SeriesCount, instant(q.ObservedStart), instant(q.ObservedEnd), p.FreshnessSeconds), Inventory: fmt.Sprintf("Inventory available: %t · eligible %d · observed %d · excluded %d", a.Evidence.Inventory.Available, a.Evidence.Inventory.Eligible, a.Evidence.Inventory.Observed, a.Evidence.Inventory.Excluded)}
			if oomOnly(a) {
				evidence.Quality = fmt.Sprintf("OOM-based sizing: usage history, sample count, coverage, and usage freshness were not required. Recorded usage: %.2fh · %d observations · %.1f%% coverage · %d samples across %d series. Missing usage is not an observed zero.", q.ObservedIntervalHours, q.ObservationCount, 100*q.Coverage, q.SampleCount, q.SeriesCount)
			}
			if a.DecisionTrace != nil {
				evidence.Source = sourceText(a.DecisionTrace.Source)
				for _, attempt := range a.DecisionTrace.Fallbacks {
					text := "Fallback " + attempt.Release + ": selected"
					if !attempt.Selected {
						text = "Fallback " + attempt.Release + ": rejected"
						for _, reason := range attempt.Reasons {
							text += "; " + reasonText(reason)
						}
					}
					evidence.Reasons = append(evidence.Reasons, text)
				}
			}
			if a.RolloutFallback != nil {
				evidence.Reasons = append(evidence.Reasons, "Sizing uses a previous release. Current rollout evidence:")
				for _, reason := range a.RolloutFallback.CurrentDataQuality.Reasons {
					evidence.Reasons = append(evidence.Reasons, reasonText(reason))
				}
			}
			for _, reason := range append(append([]analysis.Reason(nil), q.Reasons...), a.Notices...) {
				evidence.Reasons = append(evidence.Reasons, reasonText(reason))
			}
			if a.MemoryLeak != nil {
				evidence.Reasons = append(evidence.Reasons, fmt.Sprintf("Memory-leak advisory: %s (does not change sizing).", a.MemoryLeak.Status))
			}
			d.Resources = append(d.Resources, evidence)
		}
	}
	for _, comparison := range row.Scan.ReleaseComparisons {
		r := comparison.Release
		role := "Comparison only"
		if r.Current {
			role = "Current rollout"
		}
		for _, resource := range comparison.SizingResources {
			role += " · sizing source for " + string(resource)
		}
		cpu, mem := "unavailable", "unavailable"
		if comparison.CPU.AggregatedUsage.Available {
			cpu = native(model.CPU, comparison.CPU.AggregatedUsage.Value)
		}
		if comparison.Memory.AggregatedUsage.Available {
			mem = native(model.Memory, comparison.Memory.AggregatedUsage.Value)
		}
		d.Releases = append(d.Releases, Release{ID: r.ID, Name: r.Name, Image: r.Image, Revision: r.Revision, When: instant(r.CreatedAt), Role: role, CPU: cpu, Memory: mem, Quality: fmt.Sprintf("CPU: %.2fh, %.1f%% coverage, %d samples (%s — %s). Memory: %.2fh, %.1f%% coverage, %d samples (%s — %s), %d OOM kills.", comparison.CPU.HistoryHours, comparison.CPU.Coverage*100, comparison.CPU.SampleCount, instant(comparison.CPU.ObservedStart), instant(comparison.CPU.ObservedEnd), comparison.Memory.HistoryHours, comparison.Memory.Coverage*100, comparison.Memory.SampleCount, instant(comparison.Memory.ObservedStart), instant(comparison.Memory.ObservedEnd), comparison.Memory.OOMKills)})
	}
	cfg := config.Default(run.Strategy)
	cfg.PrometheusLabel, cfg.PrometheusClusterLabel = row.Connection.ClusterLabel, row.Connection.ClusterValue
	cfg.UseOOMKillData = row.Connection.OOM
	d.SetQueries(row, cfg)
	technical, _ := json.MarshalIndent(row, "", "  ")
	d.Technical = string(technical)
	d.OOMEvents = savedOOMEvents(row)
	if len(d.OOMEvents) > 0 {
		d.ChartStatus = "Saved OOM events and allocation reference lines. Usage samples have not been fetched."
		d.setCharts(row, strategy.Metrics{WindowStart: row.WindowStart, EvaluationTime: row.EvaluationTime})
	}
	return d
}
func sourceText(s analysis.UsageSource) string {
	if s.Method == "OOM kill" {
		return fmt.Sprintf("OOMKilled event · current rollout %s · %s", s.Release, instant(s.Timestamp))
	}
	text := s.Method
	if s.Percentile > 0 {
		text += fmt.Sprintf(" (P%g)", s.Percentile)
	}
	return fmt.Sprintf("%s · release %s · series %s · %s", text, s.Release, s.SeriesID, instant(s.Timestamp))
}
func reasonText(r analysis.Reason) string {
	messages := map[analysis.Reason]string{
		analysis.ReasonNoMaterialChange:      "The change did not meet both absolute and relative change thresholds.",
		analysis.ReasonLimitDisabled:         "Limit changes are disabled by the strategy.",
		analysis.ReasonBounds:                "A size bound or request/limit relationship constrained the candidate.",
		analysis.ReasonInsufficientHistory:   "The observed history is shorter than the required minimum.",
		analysis.ReasonInsufficientSamples:   "There are too few distinct observation times.",
		analysis.ReasonSparseCoverage:        "Metric coverage is below the required minimum.",
		analysis.ReasonMissingUsage:          "No usable metric samples were available.",
		analysis.ReasonStaleUsage:            "The latest usage sample is too old.",
		analysis.ReasonMissingIdentity:       "Current workload or rollout identity is missing.",
		analysis.ReasonAmbiguousIdentity:     "Workload or rollout identity is ambiguous.",
		analysis.ReasonUnknownSeriesIdentity: "Some metric series cannot be attributed to a rollout.",
		analysis.ReasonUnknownReleaseStart:   "The rollout start time is unknown.",
		analysis.ReasonInferredReleaseStart:  "The rollout start was inferred from observations.",
		analysis.ReasonStaleIdentity:         "The workload identity observation is stale.",
		analysis.ReasonMissingRequest:        "The current request could not be established consistently.",
		analysis.ReasonMissingLimit:          "The current limit could not be established consistently.",
		analysis.ReasonStaleRequest:          "The current request observation is stale.",
		analysis.ReasonStaleLimit:            "The current limit observation is stale.",
		analysis.ReasonUnknownInventory:      "Pod inventory is unavailable; reductions are blocked.",
		analysis.ReasonStaleInventory:        "Pod inventory is stale; reductions are blocked.",
		analysis.ReasonIncompleteInventory:   "Freshly observed pods do not cover the eligible inventory; reductions are blocked.",
		analysis.ReasonExcludedContainers:    "Some containers were excluded; reductions are blocked.",
		analysis.ReasonOOMKillDetected:       "OOM kills were observed in the selected evidence window.",
		analysis.ReasonOOMLimitUnknown:       "An OOM event has no known event-time memory limit; fallback sizing applies and reductions are blocked.",
		analysis.ReasonPreviousReleaseUsage:  "An earlier rollout supplies the sizing usage.",
	}
	if text, ok := messages[r]; ok {
		return text
	}
	return strings.ReplaceAll(string(r), "-", " ") + "."
}
func (d Document) Text(detailed bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "KEDR · Explain row %d\n%s\nContext: %s · Strategy: %s\nSaved: %s · Run: %s\nWindow: %s\n", d.Row, d.Target, d.Cluster, d.Strategy, d.ScanTime, d.RunID, d.Window)
	for _, card := range d.Cards {
		fmt.Fprintf(&b, "\n%s: %s → %s [%s]\n  %s\n", card.Title, card.Current, card.Recommended, card.StatusLabel(), card.Reason)
		if card.Delta != "" {
			fmt.Fprintf(&b, "  %s\n", card.Delta)
		}
		if detailed {
			if card.Source != "" {
				fmt.Fprintf(&b, "  Source: %s\n", card.Source)
			}
			for _, step := range card.Steps {
				fmt.Fprintf(&b, "  %s %s: %s\n", step.Rule, step.Outcome, step.Detail)
			}
		}
	}
	if detailed {
		for _, r := range d.Resources {
			fmt.Fprintf(&b, "\n%s evidence [%s]\n%s\n%s\n", r.Name, r.Status, r.Quality, r.Inventory)
			for _, reason := range r.Reasons {
				fmt.Fprintf(&b, "  %s\n", reason)
			}
		}
		for _, r := range d.Releases {
			fmt.Fprintf(&b, "\nRollout %s (%s), revision %d, %s\n  %s · %s\n  CPU %s · memory %s\n  %s\n", r.Name, r.ID, r.Revision, r.When, r.Image, r.Role, r.CPU, r.Memory, r.Quality)
		}
	}
	for _, warning := range d.Warnings {
		fmt.Fprintf(&b, "\nNote: %s\n", warning)
	}
	return b.String()
}

func (d *Document) AddMetrics(row runstore.Row, metrics strategy.Metrics, warnings []string) {
	d.RetrievedAt = time.Now().UTC().Format(time.RFC3339)
	d.Warnings = append(d.Warnings, warnings...)
	digest, err := runstore.MetricsDigest(metrics)
	d.ChartStatus = "Unverifiable: no compatible saved metric fingerprint."
	if err == nil && row.Digest != "" && strings.HasPrefix(row.Digest, "normalized-v1:") && row.Scan.Analysis != nil && row.Scan.Analysis.DetectorVersion == analysis.ResourceRightSizeDetectorVersion {
		if digest == row.Digest && len(warnings) == 0 {
			d.ChartStatus = "Verified: retrieved observations match the saved metric fingerprint."
		} else {
			d.ChartStatus = "Changed or incomplete: retrieved observations differ from the saved scan. The original decision is preserved."
		}
	}
	if len(metrics.CPU) == 0 && len(metrics.Memory) == 0 && len(d.OOMEvents) > 0 {
		d.ChartStatus = "No historical usage samples returned. Saved OOM events and allocation reference lines are shown. The original decision is preserved."
	}
	d.setCharts(row, metrics)
}

func (d *Document) setCharts(row runstore.Row, metrics strategy.Metrics) {
	d.Charts = nil
	for _, r := range model.ResourceTypes {
		chart := Chart{Resource: string(r), Unit: "MiB"}
		scale := 1.0 / (1024 * 1024)
		rows := metrics.Memory
		if r == model.CPU {
			chart.Unit = "mCPU"
			scale = 1
			rows = metrics.CPU
		} else {
			chart.Events = append(chart.Events, savedOOMEvents(row)...)
		}
		sizingRelease := ""
		if row.Scan.Analysis != nil {
			for _, a := range row.Scan.Analysis.Results {
				if string(a.Resource) != string(r) {
					continue
				}
				if a.DecisionTrace != nil {
					sizingRelease = a.DecisionTrace.Source.Release
				}
				if a.Evidence.AggregatedUsage.Available && !oomOnly(&a) {
					chart.Lines = append(chart.Lines, Line{Label: "Saved sizing aggregate", Value: a.Evidence.AggregatedUsage.Value * scale, Style: "aggregate"})
					chart.Events = append(chart.Events, Event{Time: a.Evidence.AggregatedUsage.Timestamp, Label: "Saved sizing sample", Kind: "sizing"})
				}
				if a.OOMAdjustment != nil {
					chart.Lines = append(chart.Lines, Line{Label: "OOM floor before bounds", Value: a.OOMAdjustment.OOMRequestFloorBytes * scale, Style: "oom"})
				}
			}
		}
		for _, s := range rows {
			points, err := analysis.NormalizeSamples(s, metrics.WindowStart, metrics.EvaluationTime)
			if err != nil {
				d.Warnings = append(d.Warnings, fmt.Sprintf("%s series could not be normalized", r))
				continue
			}
			cadence := medianCadence(points)
			for i := range points {
				points[i].Value *= scale
			}
			podName := s.PodUID
			for _, p := range row.Identity.Pods {
				if p.UID == s.PodUID {
					podName = p.Name
					break
				}
			}
			// Reduce each gap-separated segment independently; never bridge missing data.
			for _, segment := range segments(points, cadence) {
				chart.Series = append(chart.Series, ChartSeries{ID: s.ID, Pod: podName, Release: s.Release, Sizing: s.Release == sizingRelease, Points: reduce(segment, 600), Cadence: cadence})
			}
		}
		for _, line := range []struct {
			name  string
			v     model.MaybeValue
			style string
		}{
			{"Request at scan", row.Scan.Object.Allocations.Requests[r], "current"}, {"Limit at scan", row.Scan.Object.Allocations.Limits[r], "current"},
			{"Recommended request", row.Scan.Recommended.Requests[r].Value, "recommended"}, {"Recommended limit", row.Scan.Recommended.Limits[r].Value, "recommended"},
		} {
			if line.v.Set && !line.v.Unknown {
				v := line.v.Value
				if r == model.CPU {
					v *= 1000
				}
				chart.Lines = append(chart.Lines, Line{Label: line.name, Value: v * scale, Style: line.style})
			}
		}
		for _, release := range row.Scan.ReleaseComparisons {
			if release.Release.CreatedAt > 0 {
				chart.Events = append(chart.Events, Event{Time: release.Release.CreatedAt, Label: "Release created: " + release.Release.Name, Kind: "release"})
			}
		}
		if row.Scan.Analysis != nil {
			for _, a := range row.Scan.Analysis.Results {
				if string(a.Resource) == string(r) && a.Evidence.Identity.ReleaseStartInferred {
					chart.Events = append(chart.Events, Event{Time: a.Evidence.Identity.ReleaseStartedAt, Label: "Inferred current rollout start", Kind: "release"})
				}
			}
		}
		if len(chart.Series) > 0 || len(chart.Lines) > 0 || r == model.Memory && len(d.OOMEvents) > 0 {
			d.Charts = append(d.Charts, chart)
		}
	}
}
func medianCadence(points []analysis.Sample) float64 {
	if len(points) < 2 {
		return 0
	}
	gaps := make([]int64, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		gaps = append(gaps, points[i].Timestamp-points[i-1].Timestamp)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return float64(gaps[(len(gaps)-1)/2])
}
func segments(points []analysis.Sample, cadence float64) [][]analysis.Sample {
	var result [][]analysis.Sample
	start := 0
	for i := 1; i < len(points); i++ {
		if cadence > 0 && float64(points[i].Timestamp-points[i-1].Timestamp) > cadence*3 {
			result = append(result, points[start:i])
			start = i
		}
	}
	if start < len(points) {
		result = append(result, points[start:])
	}
	return result
}

// reduce preserves the min/max and endpoints of time buckets in timestamp order.
func reduce(points []analysis.Sample, buckets int) []analysis.Sample {
	if len(points) <= buckets*4 {
		return points
	}
	width := max(int64(1), (points[len(points)-1].Timestamp-points[0].Timestamp)/int64(buckets)+1)
	var out []analysis.Sample
	for i := 0; i < len(points); {
		end := i + 1
		bucket := (points[i].Timestamp - points[0].Timestamp) / width
		lo, hi := i, i
		for end < len(points) && (points[end].Timestamp-points[0].Timestamp)/width == bucket {
			if points[end].Value < points[lo].Value {
				lo = end
			}
			if points[end].Value > points[hi].Value {
				hi = end
			}
			end++
		}
		ids := []int{i, lo, hi, end - 1}
		sort.Ints(ids)
		last := -1
		for _, id := range ids {
			if id != last {
				out = append(out, points[id])
				last = id
			}
		}
		i = end
	}
	return out
}

func (d *Document) SetQueries(row runstore.Row, cfg *config.Config) {
	d.Queries = nil
	for _, metric := range prom.MetricsForStrategy(cfg) {
		d.Queries = append(d.Queries, prom.BuildMetricQuery(metric, row.Object(), cfg))
	}
}

func (c Card) StatusLabel() string {
	if c.Outcome == "disabled" {
		return "retained by policy"
	}
	return c.Outcome
}
func humanLabel(key string) string {
	var b strings.Builder
	for _, r := range key {
		if unicode.IsUpper(r) {
			b.WriteRune(' ')
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

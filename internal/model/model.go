// Package model contains the stable recommendation and report data model.
package model

import (
	"encoding/json"
	"math"
	"sort"

	"github.com/kedify/recommender/analysis"
)

type ResourceType string

const (
	CPU    ResourceType = "cpu"
	Memory ResourceType = "memory"
)

var ResourceTypes = []ResourceType{CPU, Memory}

type MaybeValue struct {
	Value   float64
	Set     bool
	Unknown bool
}

func Number(v float64) MaybeValue { return MaybeValue{Value: v, Set: true} }
func Unset() MaybeValue           { return MaybeValue{} }
func Unknown() MaybeValue         { return MaybeValue{Unknown: true} }

func (v MaybeValue) Interface() any {
	if v.Unknown || math.IsNaN(v.Value) {
		return "?"
	}
	if !v.Set {
		return nil
	}
	return v.Value
}

func (v MaybeValue) MarshalJSON() ([]byte, error) { return json.Marshal(v.Interface()) }

type Allocations struct {
	Requests map[ResourceType]MaybeValue `json:"requests" yaml:"requests"`
	Limits   map[ResourceType]MaybeValue `json:"limits" yaml:"limits"`
	Info     map[ResourceType]*string    `json:"info" yaml:"info"`
}

func EmptyAllocations() Allocations {
	return Allocations{
		Requests: map[ResourceType]MaybeValue{CPU: Unset(), Memory: Unset()},
		Limits:   map[ResourceType]MaybeValue{CPU: Unset(), Memory: Unset()},
		Info:     map[ResourceType]*string{},
	}
}

type Pod struct {
	Name               string      `json:"name" yaml:"name"`
	Deleted            bool        `json:"deleted" yaml:"deleted"`
	UID                string      `json:"-" yaml:"-"`
	Release            string      `json:"-" yaml:"-"`
	CreatedAt          int64       `json:"-" yaml:"-"`
	ContainerStartedAt int64       `json:"-" yaml:"-"`
	ContainerID        string      `json:"-" yaml:"-"`
	EndedAt            int64       `json:"-" yaml:"-"`
	Image              string      `json:"-" yaml:"-"`
	Allocations        Allocations `json:"-" yaml:"-"`
}

// Release describes a ReplicaSet or controller revision, newest first in reports.
// Workload ownership is scoped by cluster, namespace, kind and name, not UID.
type Release struct {
	ID        string `json:"id" yaml:"id"`
	Name      string `json:"name" yaml:"name"`
	Image     string `json:"image,omitempty" yaml:"image,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty" yaml:"createdAt,omitempty"`
	Revision  int64  `json:"revision,omitempty" yaml:"revision,omitempty"`
	Current   bool   `json:"current" yaml:"current"`
}

// ReleaseUsage is descriptive evidence, not a recommendation for an old image.
// CPU values are millicores; memory values are bytes (the analyzer's native units).
type ReleaseUsage struct {
	AggregatedUsage analysis.Signal `json:"aggregatedUsage" yaml:"aggregatedUsage"`
	ObservedStart   int64           `json:"observedStart" yaml:"observedStart"`
	ObservedEnd     int64           `json:"observedEnd" yaml:"observedEnd"`
	HistoryHours    float64         `json:"historyHours" yaml:"historyHours"`
	Coverage        float64         `json:"coverage" yaml:"coverage"`
	SampleCount     int             `json:"sampleCount" yaml:"sampleCount"`
	OOMKills        int             `json:"oomKills,omitempty" yaml:"oomKills,omitempty"`
}

type ReleaseComparison struct {
	Release Release      `json:"release" yaml:"release"`
	CPU     ReleaseUsage `json:"cpu" yaml:"cpu"`
	Memory  ReleaseUsage `json:"memory" yaml:"memory"`
}

type HPA struct {
	MinReplicas         *int32   `json:"min_replicas" yaml:"min_replicas"`
	MaxReplicas         int32    `json:"max_replicas" yaml:"max_replicas"`
	CurrentReplicas     *int32   `json:"current_replicas" yaml:"current_replicas"`
	DesiredReplicas     int32    `json:"desired_replicas" yaml:"desired_replicas"`
	TargetCPUPercent    *float64 `json:"target_cpu_utilization_percentage" yaml:"target_cpu_utilization_percentage"`
	TargetMemoryPercent *float64 `json:"target_memory_utilization_percentage" yaml:"target_memory_utilization_percentage"`
}

type Object struct {
	Cluster     *string           `json:"cluster" yaml:"cluster"`
	Name        string            `json:"name" yaml:"name"`
	Container   string            `json:"container" yaml:"container"`
	Pods        []Pod             `json:"pods" yaml:"pods"`
	HPA         *HPA              `json:"hpa" yaml:"hpa"`
	Namespace   string            `json:"namespace" yaml:"namespace"`
	Kind        string            `json:"kind" yaml:"kind"`
	Allocations Allocations       `json:"allocations" yaml:"allocations"`
	Warnings    []string          `json:"warnings" yaml:"warnings"`
	Labels      map[string]string `json:"labels" yaml:"labels"`
	Annotations map[string]string `json:"annotations" yaml:"annotations"`
	Releases    []Release         `json:"releases,omitempty" yaml:"releases,omitempty"`

	Selector             string             `json:"-" yaml:"-"`
	UID                  string             `json:"-" yaml:"-"`
	GroupedJobs          []string           `json:"-" yaml:"-"`
	GroupingExpr         string             `json:"-" yaml:"-"`
	Release              string             `json:"-" yaml:"-"`
	ObservedAt           int64              `json:"-" yaml:"-"`
	ReleaseStartedAt     int64              `json:"-" yaml:"-"`
	ReleaseStartInferred bool               `json:"-" yaml:"-"`
	IdentityAmbiguous    bool               `json:"-" yaml:"-"`
	InventoryAvailable   bool               `json:"-" yaml:"-"`
	ExcludedPods         int                `json:"-" yaml:"-"`
	Generation           int64              `json:"-" yaml:"-"`
	ObservedGeneration   int64              `json:"-" yaml:"-"`
	TemplateHash         string             `json:"-" yaml:"-"`
	Image                string             `json:"-" yaml:"-"`
	OOMKills             []analysis.OOMKill `json:"-" yaml:"-"`
}

func (o Object) CurrentPods() int {
	n := 0
	for _, pod := range o.Pods {
		if !pod.Deleted {
			n++
		}
	}
	return n
}

func (o Object) DeletedPods() int { return len(o.Pods) - o.CurrentPods() }

type Severity string

const (
	SeverityUnknown  Severity = "UNKNOWN"
	SeverityGood     Severity = "GOOD"
	SeverityOK       Severity = "OK"
	SeverityWarning  Severity = "WARNING"
	SeverityCritical Severity = "CRITICAL"
)

type RecommendationValue struct {
	Value    MaybeValue `json:"value" yaml:"value"`
	Severity Severity   `json:"severity" yaml:"severity"`
}

type Recommendation struct {
	Requests map[ResourceType]RecommendationValue `json:"requests" yaml:"requests"`
	Limits   map[ResourceType]RecommendationValue `json:"limits" yaml:"limits"`
	Info     map[ResourceType]*string             `json:"info" yaml:"info"`
}

type Scan struct {
	Object              Object                  `json:"object" yaml:"object"`
	Recommended         Recommendation          `json:"recommended" yaml:"recommended"`
	Severity            Severity                `json:"severity" yaml:"severity"`
	Analysis            *analysis.Output        `json:"analysis,omitempty" yaml:"analysis,omitempty"`
	ReleaseComparisons  []ReleaseComparison     `json:"releaseComparisons,omitempty" yaml:"releaseComparisons,omitempty"`
	SuppressedResources map[ResourceType]string `json:"suppressedResources,omitempty" yaml:"suppressedResources,omitempty"`
}

type StrategyData struct {
	Name     string         `json:"name" yaml:"name"`
	Settings map[string]any `json:"settings" yaml:"settings"`
}

type Report struct {
	Scans          []Scan           `json:"scans" yaml:"scans"`
	Score          int              `json:"score" yaml:"score"`
	Resources      []string         `json:"resources" yaml:"resources"`
	Description    string           `json:"description" yaml:"description"`
	Strategy       StrategyData     `json:"strategy" yaml:"strategy"`
	Errors         []map[string]any `json:"errors" yaml:"errors"`
	ClusterSummary map[string]any   `json:"clusterSummary" yaml:"clusterSummary"`
	Config         any              `json:"config" yaml:"config"`
}

func (r *Report) CalculateScore() {
	if len(r.Scans) == 0 {
		r.Score = 0
		return
	}
	cost := 0.0
	for _, scan := range r.Scans {
		switch scan.Severity {
		case SeverityCritical:
			cost++
		case SeverityWarning:
			cost += 0.7
		case SeverityUnknown, SeverityGood, SeverityOK:
			continue
		}
	}
	r.Score = int((float64(len(r.Scans)) - cost) / float64(len(r.Scans)) * 100)
}

func (r Report) ScoreLetter() string {
	switch {
	case r.Score < 30:
		return "F"
	case r.Score < 55:
		return "D"
	case r.Score < 70:
		return "C"
	case r.Score < 90:
		return "B"
	default:
		return "A"
	}
}

func WorstSeverity(values ...Severity) Severity {
	rank := map[Severity]int{SeverityUnknown: 0, SeverityGood: 1, SeverityOK: 2, SeverityWarning: 3, SeverityCritical: 4}
	worst := SeverityUnknown
	for _, value := range values {
		if rank[value] > rank[worst] {
			worst = value
		}
	}
	return worst
}

func SortObjects(objects []Object) {
	sort.SliceStable(objects, func(i, j int) bool {
		a, b := objects[i], objects[j]
		ac, bc := "", ""
		if a.Cluster != nil {
			ac = *a.Cluster
		}
		if b.Cluster != nil {
			bc = *b.Cluster
		}
		if ac != bc {
			return ac < bc
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Container < b.Container
	})
}

func SortObjectsFromScans(scans []Scan) {
	for i := range scans {
		sort.Strings(scans[i].Object.Warnings)
		sort.SliceStable(scans[i].Object.Pods, func(a, b int) bool {
			left, right := scans[i].Object.Pods[a], scans[i].Object.Pods[b]
			if left.Name != right.Name {
				return left.Name < right.Name
			}
			return !left.Deleted && right.Deleted
		})
	}
	sort.SliceStable(scans, func(i, j int) bool {
		a, b := scans[i].Object, scans[j].Object
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		ac, bc := "", ""
		if a.Cluster != nil {
			ac = *a.Cluster
		}
		if b.Cluster != nil {
			bc = *b.Cluster
		}
		if ac != bc {
			return ac < bc
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Container < b.Container
	})
}

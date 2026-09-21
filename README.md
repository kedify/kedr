# KEDR

KEDR is a fast, standalone Kubernetes resource recommender written entirely in Go. It reads workload configuration from Kubernetes, queries a Prometheus-compatible metrics service, and produces CPU and memory request/limit recommendations.

KEDR uses [`github.com/kedify/recommender/analysis`](https://github.com/kedify/recommender)
for all recommendation calculations through `analysis.Analyze`. Its CLI and report
layout originated as a clean-room port of [Robusta KRR](https://github.com/robusta-dev/krr),
but its recommendation semantics are no longer KRR-compatible. It has no Python
runtime or source dependency.

## Install and run

```sh
# During development, keep kedr and recommender in sibling directories.
go build -o bin/kedr ./cmd/kedr
./bin/kedr simple
```

This checkout uses a local `replace github.com/kedify/recommender => ../recommender`
to include the unreleased OOM/leak extensions. Before publishing a standalone
release, publish and pin a recommender version containing those changes and remove
the replacement. `go install ...@version` is not supported for this development
checkout's module configuration.

Many existing KRR flags still work, including underscore aliases:

```sh
alias krr=kedr
krr simple --namespace default --formatter json --logtostderr
```

Use `kedr simple --help` or `kedr simple_limit --help` for the complete option reference.

## Strategies

- `simple`: nearest-rank CPU percentile **per container lifetime**, then the maximum
  across replicas; changes CPU requests only. Existing CPU limits are retained,
  not removed. The default percentile is 95.
- `simple_limit`: the same CPU aggregation with default request percentile 66;
  CPU limits use `request × --cpu-limit-ratio` (default 5). The old `--cpu-limit`
  percentile flag is rejected with a migration message.
- Both use selected-release peak memory with `--memory-buffer-percentage`
  (default 15%), and a memory limit/request ratio of 1, subject to retained settings
  and the analyzer's safety guards.

KEDR maps its existing CPU/memory minimums (10 millicores / 100 MiB), request
percentile, memory buffer, OOM increase, and sample-count flags into the shared
policy. CPU headroom is 1. Other bounds and material-change thresholds come from
the module. No extra KRR percentile interpolation, sizing, rounding, or floors run
after `Analyze`; exact recommendations are retained, with CPU converted from the
module's millicores to report cores. Display formatting does not change stored values.

The analyzer requires seven days of observed history by default, 90% coverage,
and fresh observations. Missing historical samples reduce measured coverage but
do not independently block recommendations. `--history-duration`
controls the query window; it does **not** weaken these guards. Explicitly change
`--minimum-history-hours` when a shorter evidence requirement is appropriate.
`--points-required` applies to distinct observations, not repeated query evaluations.
CPU and memory retain native scrape samples; `--timeframe-duration` only controls
the optional OOM expression's query step.

### Release selection

Recommendations remain **per workload/container**, not per pod. Workloads are
scoped by cluster, namespace, kind and name; recreating a workload with the same
name is intentionally treated as the same workload. Pod-level observations remain
separate for CPU rates, percentiles, and memory-leak detection.

`--release-history N` retains up to N releases (default **3**) within the query
window. Only the newest/current release drives sizing, OOM adjustments, and leak
findings. Previous releases are **comparison only**: their CPU aggregate, memory
peak, observed history, coverage, and OOM counts are reported separately. There is
no blending and no fallback to an old image's sizing when a new release lacks
evidence. A new release must still satisfy `--minimum-history-hours` itself.

Deployments use ReplicaSets as release boundaries, including image **and** other
pod-template changes. Current rollout revision takes precedence over creation
time (important for rollback); available rollout-completion time is used as a
conservative start boundary. Images are included in release metadata for
explanation, not used to merge different ReplicaSets. Reusing a mutable image tag
without creating a new rollout is not automatically a new release.

StatefulSets and DaemonSets use controller revisions. Historical
`kube_pod_labels{label_controller_revision_hash="..."}` gives the precise revision
when KSM exports that label. Otherwise, image metadata can associate a historical
pod with a known revision **only if the image identifies one revision uniquely**;
this fallback is disclosed as `HistoricalReleaseIdentityUsesImage`. Ambiguous or
unmatched images are comparison-only cohorts, never relabeled as the current
revision. Export the controller-revision-hash pod label to distinguish
configuration-only rollouts reliably.

KEDR retains its HPA guard, severity, and scoring. Unless `--allow-hpa` is set,
HPA-managed resources display `?`. Raw analyzer output remains available for
auditing, with `suppressedResources` recording which results the report withheld.
Consumers acting on reports must honor that suppression. An analyzer no-action
result retains the current setting; unavailable evidence displays `?`.

### Optional diagnostics

`--use-oomkill-data` forwards actual OOMKilled termination events from Kubernetes
pod status and, when available, kube-state-metrics. Repeated observations use the
same event ID. The adapter never substitutes the current configured limit for an
unknown termination-time limit: such events use the shared analyzer's conservative
usage-based fallback and block downsizing. The existing
`--oom-memory-buffer-percentage` sets `OOMKilledCoefficient` (default 1.25).

`--detect-memory-leaks` enables the module's advisory lower-baseline trend detector.
It is off by default, does not change sizing, and can report potential growth even
when the longer sizing-history guard blocks recommendations. A potential leak is
not a diagnosis; useful allocations and growing caches can look similar.

## Workloads and metrics

Discovery supports Deployment, StatefulSet, DaemonSet, Job, CronJob, GroupedJob,
and Argo Rollout. StrimziPodSet and OpenShift DeploymentConfig are intentionally
unsupported. A GroupedJob spans independent Job UIDs and cannot currently be passed
as one trustworthy analyzer target: it is reported with unavailable identity and
`GroupedJobIdentityUnsupported`, not a pooled recommendation.

KEDR supports an explicit Prometheus URL and in-cluster discovery of Prometheus, VictoriaMetrics, Thanos, and Mimir. Authentication options include arbitrary headers, bearer tokens, custom CAs through the base64-encoded `CERTIFICATE` environment variable, Amazon Managed Prometheus SigV4/role assumption, Coralogix, and OpenShift service-account tokens. The `--openshift` flag controls only Prometheus authentication; it does not enable DeploymentConfig discovery.

The Prometheus-compatible service should expose:

- `container_cpu_usage_seconds_total`
- `container_memory_working_set_bytes`
- Historical ownership: `kube_pod_owner`, and `kube_replicaset_owner` for
  Deployments/Rollouts. Ownership is joined by namespace and owner name; deleted
  pods and deleted ReplicaSets are included when their KSM series are retained.
- Historical metadata: `kube_pod_created`, `kube_replicaset_created`, and
  `kube_pod_container_info`; `kube_pod_labels` supplies controller revisions when
  that pod label is allowed in KSM.
- For optional OOM history: `kube_pod_container_status_last_terminated_reason`
  and `kube_pod_container_status_last_terminated_timestamp`.

Usage queries fetch individual CPU counters and memory gauges using instant
range-vector queries, not precomputed rates/percentiles/maxima or a resampled
`query_range` grid. Every original scrape timestamp is preserved. Pod batches and
six-hour time chunks bound query sizes. Replicated scrape sources
are selected deterministically, preferring `job="kubelet"`; they are not pooled.

The Kubernetes service account needs access to workload objects, pods,
ReplicaSets, ControllerRevisions (for StatefulSets/DaemonSets), and Jobs for
CronJob ownership. Pod UIDs, revision identity, pod creation time, and container
lifetime IDs are retained when available. When cAdvisor
does not expose a lifetime ID, only the API-verified current container lifetime is
used. Unobserved controller generations and inconsistent replica resource settings
block affected recommendations.

The API supplies the live inventory, while KSM supplies historical membership.
Pod replacement within the same release therefore does not reset its history.
Missing KSM ownership produces a specific `HistoricalOwnershipUnavailable`
warning; the unconditional `HistoricalPodsRequireVerifiedIdentity` warning has
been removed. Jobs/CronJobs retain the API-based adapter. Without an activation
boundary, release-start inference remains subject to the shared analyzer's
documented rollback limitations.

## Reports

The default terminal table uses the Charm Bubble Tea, Bubbles, and Lip Gloss stack. Non-terminal output never contains animation or ANSI escapes.

Table resource quantities are rounded to whole numbers. CPU always uses the
nearest millicore (`m`), including values above one core (`542.55m` becomes `543m`);
memory uses binary units.
Diff columns show total request changes across current pods, while parenthesized
changes are per container. Diffs are calculated before display rounding, so small
rounding differences can occur when multiplying the displayed per-container value.
Structured and raw reports retain the original recommendation precision.

Available formatters are `table`, `json`, `yaml`, `pprint`, `csv`, `csv-raw`, and `html`. Write a copy with `--fileoutput PATH`, or use `--fileoutput-dynamic` to create `kedr-YYYYMMDDhhmmss.FORMAT`.

```sh
kedr simple --quiet --formatter json > report.json
kedr simple --formatter csv --fileoutput report.csv
```

JSON/YAML retain the existing report layout and add each scan's full `analysis`
output (effective policy, detector/policy versions, evidence, quality reasons,
OOM adjustments, and optional leak findings), `object.releases`, and
`releaseComparisons` (CPU in millicores; memory in bytes). CSV adds a `Notes` column;
table/HTML output also identifies sizing versus comparison releases and exposes
diagnostic reasons and notices. Insufficient history includes observed and required
hours. Credentials remain
masked in serialized configuration.

## Deliberately excluded

KEDR does not contain Robusta SaaS publishing, a web UI, HolmesGPT, Slack delivery, Azure Blob/Teams delivery, the KRR enforcer, Helm charts, or runtime-loaded custom Python strategies and formatters. Flags belonging to these removed features are rejected as unknown.

## Development

The supported toolchain is Go 1.27 or newer.

```sh
go test ./...
go test -race ./...
make build
```

Integration tests use Kubernetes fake clients and local HTTP servers and do not require a cluster. Before a release, test against at least one real cluster with kube-state-metrics and cAdvisor data.

## License and attribution

KEDR's own code is MIT licensed; see [NOTICE](NOTICE) and [LICENSE](LICENSE).
The linked Kedify Recommender module has separate commercial/public-source terms;
see that module's `LICENSE` and `PUBLIC_SOURCE_LICENSE`. Its inclusion does not
make the dependency MIT licensed. Include applicable dependency license notices
when distributing binaries.

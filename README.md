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
to include the unreleased OOM/leak and decision-trace extensions. Before publishing a standalone
release, publish and pin a recommender version containing those changes and remove
the replacement. `go install ...@version` is not supported for this development
checkout's module configuration.

Many existing KRR flags still work, including underscore aliases:

```sh
alias krr=kedr
krr simple --namespace default --formatter json --logtostderr
```

Use `kedr simple --help` or `kedr simple_limit --help` for the complete option reference.

Table output includes a saved-run reference and an `explain` hint. Add `--explain`
to include the diff legend, workload diagnostics, rollout comparisons, and score.
By default, the table shows only rows with at least one visible request or limit
change. Rows with only unchanged or unknown (`?`) recommendations are hidden;
`--full` shows every row. Unchanged request and limit cells (including
`unset -> unset`) stay blank in both modes. Other output formats and saved scans retain all rows.
Row numbers can have gaps because they keep their saved-run mapping for `kedr explain`.
With `--verbose`, the calculating indicator becomes a single
`Calculating recommendations` log line so it does not interrupt the logs.
Verbose logs also show the Kubernetes context, selected Prometheus URL (and whether
it was auto-discovered), and collection diagnostics such as missing CPU or memory
metrics. URL credentials and query parameters are omitted.
Discovery lists matching Prometheus, Mimir, VictoriaMetrics, and Thanos services
and ingresses. A single endpoint is selected automatically; multiple endpoints
open a numbered choice showing the backend, namespace, resource name, and URL.
Enter a number to select one or `q` to cancel. The prompt uses stderr, so reports
can still be redirected. Without an interactive stdin and stderr, kedr lists the
candidates and exits with instructions to supply `--prometheus-url <URL>`.
That flag bypasses discovery and the prompt, including in scripts. Discovered
Kubernetes service-proxy URLs can be passed directly and use the context's credentials.
When every applicable workload in a context is missing the same metrics, kedr
prints one warning with setup guidance, even without `--verbose`. KSM ownership
metrics require kube-state-metrics to be installed and scraped; CPU and memory
usage require kubelet/cAdvisor scraping. The warning also calls out endpoint,
tenant, filtering, and Mimir remote-write configuration. These shared problems
are summarized instead of repeating their codes in verbose logs. Detailed codes
remain in saved scans and reports, and the summary is included in JSON/YAML errors.

## Explain a recommendation

```sh
kedr simple
kedr explain 1                          # Explain row 1 of the latest saved scan
kedr explain 1 --offline                # Saved decision, without metric queries
kedr explain 1 --format text            # Full terminal walkthrough, no network
kedr explain 1 --output ./explain.html  # Choose the HTML destination
kedr explain 1 --run <run-id>            # Explain a row from an older saved scan
```

`explain` prints the selected workload, context, scan timestamp, and all four
request/limit decisions. By default it also creates a standalone HTML report with
interactive CPU/memory charts, calculation steps, quality guards, and rollout
comparisons. Click the terminal file link (or open the printed path) to view it;
**kedr never launches a browser**. Charts support hover, synchronized time zoom,
and pod/release/reference-line toggles. No external web assets are required.

Rows retain the displayed order: workload name, cluster, namespace, kind, then
container. Numbers belong to a particular saved run, not permanently to a workload.
Each completed scan saves its exact row mapping, allocation states, analyzer policy
and decision traces, original query window, pod identities/lifetimes, and release
metadata. Metric samples are **not** stored by scanning. The latest 10 runs live in
`os.UserCacheDir()/kedr/runs` (`~/Library/Caches/kedr/runs` on macOS;
`$XDG_CACHE_HOME/kedr/runs` or `~/.cache/kedr/runs` on Linux). Generated explanations
live alongside their run by default and are removed with it during retention cleanup.
Use `--output` to preserve a report elsewhere, or `--no-save` on scans to opt out.
Saved files use private permissions and exclude arbitrary workload annotations,
authentication headers/tokens, and URLs containing user credentials, query strings,
or fragments. Run references do not add text to machine-readable scan output.

For HTML charts, kedr reconnects to the saved metrics source/context and requests
**the original time window and pod identities**. It does not analyze today's
workload instead. A metric fingerprint indicates whether retrieved observations
match the saved scan. Changed, expired, or unavailable data never changes the
original recommendation; missing charts show an explanation and the saved rationale
remains readable. Rollout history is limited to releases retained by the original
scan, including potentially different CPU and memory fallback sources.

Credentials are resolved again at explanation time. Existing connection flags are
supported, for example `--kubeconfig`, `--prometheus-url`,
`--prometheus-auth-header`, `--prometheus-headers`, and AWS authentication flags.
Supply secret headers again if the source requires them. An explicit replacement
endpoint does not inherit the original Kubernetes bearer token. A changed or missing
saved Kubernetes context produces a chart diagnostic rather than silently using the
current context. `--offline` and `--format text` need no credentials.

The report explains the strategy's actual behavior, including retained limits,
material-change thresholds, HPA suppression, insufficient evidence, OOM adjustments,
and bounds. `simple` retains CPU limits; `explain` does not introduce limit removal.

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

Known unset requests and limits can receive initial values below the analyzer's
minimum-change thresholds. These thresholds apply only to existing numeric
settings. Evidence and safety guards still apply; `simple` continues to leave CPU
limits unchanged, while `simple_limit` can initialize them.

The analyzer requires one hour of observed history by default, 30 distinct
observation times, 90% coverage,
and fresh observations. Missing historical samples reduce measured coverage but
do not independently block recommendations. `--history-duration-hours` defaults to
48 hours and controls the query window; it does **not** weaken these guards. Explicitly change
`--minimum-history-hours` when a shorter evidence requirement is appropriate.
`--points-required` applies to distinct observations, not repeated query evaluations.
CPU and memory retain native scrape samples; `--timeframe-duration` only controls
the optional OOM expression's query step.

### Release selection

Recommendations remain **per workload/container**, not per pod. Workloads are
scoped by cluster, namespace, kind and name; recreating a workload with the same
name is intentionally treated as the same workload. Pod-level observations remain
separate for CPU rates, percentiles, and memory-leak detection.

`--release-history N` retains up to N rollouts within the query window, including
current (default **4**, allowed **1–4**). CPU and memory each try the current
rollout first. If its usage is missing or has insufficient history or samples,
the analyzer tries the previous rollout, then the one before it, stopping after
**three previous rollouts**. Each candidate must independently pass the history,
sample-count, coverage, and historical freshness checks. Short histories are
never combined. Set `--release-history 1` to disable historical fallback.

Recommendations still target the current workload/container and use its current
resource settings and identity. Current HPA, inventory, OOM, and material-change
guards still apply; missing current pod observations can still block downsizing.
OOM adjustments and leak findings remain scoped to the current rollout. Unknown
image-only cohorts cannot supply fallback usage.

Reports identify the selected sizing source for each resource. Raw analysis adds
`rolloutFallback` with the source rollout, historical evaluation time, and the
current rollout's failed evidence checks. Historical samples keep their original
timestamps and are evaluated at the end of their observed segment, capped by the
current rollout's start. Release comparisons keep their own usage/history, with
`sizingResources` identifying which supply recommendations. Unselected rollouts
remain comparison only. The 30-observation default allows a rollout with one hour
of minute-level scrapes to qualify, including modest gaps within the 90% coverage
guard. Pod uptime alone does not guarantee enough observed history or coverage.
An explicit `--points-required 100` still requires 100 distinct observation times.

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
same event ID. Current-rollout OOM evidence can trigger a memory increase without
usage samples, minimum history, coverage, or fresh usage. The failed limit is used
when known; otherwise the analyzer explicitly falls back to the current memory
limit, then the current request, then qualifying usage. Repeated events do not
compound the multiplier. Unknown failed limits block reductions. Existing resource
bounds and change thresholds still apply, including the default 100Mi minimum.
`--oom-memory-buffer-percentage` defaults to 50 (a multiplier of 1.5).
With this flag enabled, the table ends with `OOMKilled`, showing the latest
selected event as `Yes (59s ago)`, `Yes (5m ago)`, or a local clock time for events
older than 15 minutes. Earlier days include `yesterday` or the date. Rows with an
OOM event remain visible even when no resource change can be recommended.
KSM exposes the last termination reason/time and a restart counter for all causes,
so the table retains the timestamp instead of presenting an incomplete OOM count.
`kedr explain` plots the saved OOM events and memory allocation reference lines
even offline or when historical usage is unavailable. The walkthrough shows the
OOM sizing base, multiplier, resulting floor, and any minimum/maximum bounds;
for example, `50Mi × 1.5 = 75Mi`, raised to the configured `100Mi` minimum.
Event markers and calculations always come from the original scan.

`--detect-memory-leaks` enables the module's advisory lower-baseline trend detector.
It is off by default, does not change sizing, and uses its own six-hour minimum history, independent of the one-hour sizing
default. A customized longer sizing guard can still block sizing while leak
detection reports potential growth. A potential leak is
not a diagnosis; useful allocations and growing caches can look similar.

## Workloads and metrics

Discovery supports Deployment, StatefulSet, DaemonSet, Job, CronJob, GroupedJob,
Argo Rollout, and standalone pods. StrimziPodSet and OpenShift DeploymentConfig are intentionally
unsupported. A GroupedJob spans independent Job UIDs and cannot currently be passed
as one trustworthy analyzer target: it is reported with unavailable identity and
`GroupedJobIdentityUnsupported`, not a pooled recommendation.

Pods with no owner references, such as those created with `kubectl run`, are included
by default with `Kind = Standalone Pod`, one row per regular container. Mirror/static
pods are excluded. Use `--resource Pod` (or `--resource StandalonePod`) to scan only
standalone pods; namespace and label filters apply as usual. The pod UID and creation
time define its history boundary. Container restarts within that pod remain eligible;
an older pod with the same name is not a previous rollout or fallback source. These
pods use the same evidence requirements and recommendation policies as other workloads.

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
`query_range` grid. Every original scrape timestamp is preserved. Batches contain
at most 50 distinct pod names. Usage time chunks allow up to 300 pod-hours per
request (six hours for 50 pods, longer for smaller workloads). Chunks run
concurrently within the shared `--max-workers` request limit; windows entirely
outside known pod lifetimes are skipped. Optional OOM queries keep six-hour
chunks and retain observations of earlier events. Replicated scrape sources
are selected deterministically, preferring `job="kubelet"`; they are not pooled.

The Kubernetes service account needs access to workload objects, pods,
ReplicaSets, ControllerRevisions (for StatefulSets/DaemonSets), and Jobs for
CronJob ownership. Standalone pod discovery and observation need `list` and `get`
access to pods. Pod UIDs, revision identity, pod creation time, and container
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

CPU Diff and Memory Diff use separate color scales over the visible rows. Savings
range from muted grey-green to bright green; increases range from muted warm grey
through orange to bright red. The largest change of each sign is bold. Scales use
the displayed total request change across current pods, excluding unset/unknown allocations,
unchanged displayed values, and zero totals. Remaining numeric diffs outside the
scale are grey. Equal changes share
a shade; a single change of one sign uses the brightest shade.
Colors adapt to the terminal's supported palette. `--no-color` or any nonempty
`NO_COLOR` value disables all terminal colors, including headers and the progress
indicator. Redirected output and saved table files contain no ANSI styling.

Table resource quantities are rounded to whole numbers. CPU always uses the
nearest millicore (`m`), including values above one core (`542.55m` becomes `543m`);
memory uses binary units.
Diff columns show total request changes across current pods; previously unset or
unchanged requests leave the diff cell blank. OOM rows with no current pods show the
per-container memory change, so `50Mi -> 100Mi` displays `+50Mi` instead of `+0`.
Request and limit columns show per-container
values with vertically aligned arrows. Diffs are
calculated before display rounding, so small rounding differences can occur when
multiplying the displayed per-container value.
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

Use `--verbose --logtostderr` to see endpoint discovery, workload discovery, and
per-container inventory, historical metadata, usage collection, and analysis
timings. Repeatable collection/decoding benchmarks are available with:

```sh
go test -run '^$' -bench 'Benchmark(NativeHistoryDecode|HistoryRoundTrips)$' -benchmem ./internal/prometheus
```

See [performance measurements](docs/performance.md) for the bottlenecks, changes,
and a read-only cluster benchmark.

## License and attribution

KEDR's own code is MIT licensed; see [NOTICE](NOTICE) and [LICENSE](LICENSE).
The linked Kedify Recommender module has separate commercial/public-source terms;
see that module's `LICENSE` and `PUBLIC_SOURCE_LICENSE`. Its inclusion does not
make the dependency MIT licensed. Include applicable dependency license notices
when distributing binaries.

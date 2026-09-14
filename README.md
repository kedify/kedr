# KEDR

KEDR is a fast, standalone Kubernetes resource recommender written entirely in Go. It reads workload configuration from Kubernetes, queries a Prometheus-compatible metrics service, and produces CPU and memory request/limit recommendations.

KEDR is a clean-room Go port of the core CLI behavior of [Robusta KRR](https://github.com/robusta-dev/krr), based on KRR commit `d1ccf01`. It has no Python runtime or source dependency.

## Install and run

```sh
go install github.com/kedify/kedr/cmd/kedr@latest
kedr simple
```

Existing KRR command lines can use the new binary without argument changes:

```sh
alias krr=kedr
krr simple --namespace default --formatter json --logtostderr
```

Use `kedr simple --help` or `kedr simple_limit --help` for the complete option reference.

## Strategies

- `simple`: CPU request from the configured Prometheus percentile with no CPU limit; memory request and limit from peak usage plus a buffer.
- `simple_limit`: independently configurable CPU request and limit percentiles; memory uses the same peak-plus-buffer calculation.

Both strategies preserve KRR behavior for minimum values, sparse data, HPA-managed resources, OOM-kill history, rounding, severity, and scoring.

## Workloads and metrics

Supported workload types are Deployment, StatefulSet, DaemonSet, Job, CronJob, GroupedJob, and Argo Rollout. StrimziPodSet and OpenShift DeploymentConfig are intentionally unsupported.

KEDR supports an explicit Prometheus URL and in-cluster discovery of Prometheus, VictoriaMetrics, Thanos, and Mimir. Authentication options include arbitrary headers, bearer tokens, custom CAs through the base64-encoded `CERTIFICATE` environment variable, Amazon Managed Prometheus SigV4/role assumption, Coralogix, and OpenShift service-account tokens. The `--openshift` flag controls only Prometheus authentication; it does not enable DeploymentConfig discovery.

Prometheus 2.26 or newer should expose:

- `container_cpu_usage_seconds_total`
- `container_memory_working_set_bytes`
- `kube_replicaset_owner`
- `kube_pod_owner`
- `kube_pod_status_phase`

Historical pods are omitted when the ownership metrics are unavailable, but currently running pods are still used.

## Reports

The default terminal table uses the Charm Bubble Tea, Bubbles, and Lip Gloss stack. Non-terminal output never contains animation or ANSI escapes.

Available formatters are `table`, `json`, `yaml`, `pprint`, `csv`, `csv-raw`, and `html`. Write a copy with `--fileoutput PATH`, or use `--fileoutput-dynamic` to create `kedr-YYYYMMDDhhmmss.FORMAT`.

```sh
kedr simple --quiet --formatter json > report.json
kedr simple --formatter csv --fileoutput report.csv
```

JSON, YAML, and CSV retain the KRR report field and value contract. Credentials are masked in serialized configuration.

## Deliberately excluded

KEDR does not contain Robusta SaaS publishing, a web UI, HolmesGPT, Slack delivery, Azure Blob/Teams delivery, the KRR enforcer, Helm charts, or runtime-loaded custom Python strategies and formatters. Flags belonging to these removed features are rejected as unknown.

## Development

The supported toolchain is Go 1.27 or newer.

```sh
make verify
make build
```

Integration tests use Kubernetes fake clients and local HTTP servers and do not require a cluster. Before a release, test against at least one real cluster with kube-state-metrics and cAdvisor data.

## License and attribution

KEDR is MIT licensed. Its behavior and algorithms derive from Robusta KRR; see [NOTICE](NOTICE) and [LICENSE](LICENSE).

# Memory leak detection demo

This Deployment retains 64 MiB at startup and touches another 12 MiB every 30 minutes. The byte arrays stay referenced, so the process's resident working set rises in steps. The pod uses the official `python:3.13-slim-bookworm` image and needs no image build. It deliberately grows without a cap; delete the namespace after the demo.

## Run it

From the repository root, with a Kubernetes context that has Prometheus container metrics available:

```sh
kubectl apply -f examples/memory-leak/manifest.yaml
kubectl -n kedr-memory-leak-demo rollout status deployment/memory-leak-demo
kubectl -n kedr-memory-leak-demo logs deployment/memory-leak-demo --follow
```

In another terminal, after at least **7–8 hours** of uninterrupted samples:

```sh
kedr simple --namespace kedr-memory-leak-demo --detect-memory-leaks --full --explain
```

Look for `potential-memory-leak` in the memory notes. For the complete result, use `--formatter json` and inspect `scans[].analysis.results[]` where `resource` is `memory`: the `memoryLeak` object includes the status, episode counts, baselines, growth, slopes, coverage, and reasons. If saving is enabled, `kedr explain <row>` also displays the advisory status.

The detector reads native `container_memory_working_set_bytes` samples. It skips the first 30 minutes of a container lifetime, needs at least twelve usable 30-minute buckets and six hours after warmup, and requires five samples per bucket, 90% coverage, and fresh samples. It compares the lower baseline across buckets, requiring at least 64 MiB and 20% growth plus a continuing upward trend. With this workload, the first and last quarters of twelve buckets differ by roughly 108 MiB. Depending on scrape timing, rollout discovery, and coverage, a positive finding can take longer than seven hours. `--history-duration-hours` defaults to 48, while the leak detector uses at most its latest 24 hours; `--minimum-history-hours` controls sizing separately.

The 1 GiB limit allows a substantial observation window, but the process will eventually reach it and restart. A restart begins a new container lifetime and can delay detection. Run the scan before that happens and remove the demo when finished:

```sh
kubectl delete namespace kedr-memory-leak-demo
```

`potential-memory-leak` is an advisory trend finding, not proof of an application bug. KEDR does not change the recommendation because of this flag.

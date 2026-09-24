# Collection performance

The main bottleneck was Prometheus round-trip latency. The original collector
fetched CPU and memory in sequential six-hour chunks: 112 requests for a 14-day
lookback, even for a single pod. Different workloads could run concurrently,
but a small scan or the last few workloads left most workers idle.

KRR's `simple` strategy asks Prometheus for CPU percentiles, memory maxima, and
sample counts. KEDR instead downloads native counters/gauges and historical pod
metadata for its shared analyzer, which uses container lifetimes, release
identity, coverage, and optional leak detection. Moving back to KRR's aggregated
queries would change recommendation semantics.

## Changes

- Schedule time chunks concurrently, respecting the existing shared request
  limit. Merge identical source series and restore chronological sample order.
- Adapt usage windows to the number of distinct pod names, preserving the
  original maximum of 50 pods × 6 hours per response. One pod needs four usage
  requests over 14 days instead of 112. Skip chunks outside known pod lifetimes
  and avoid fetching reused pod names twice. Keep OOM observation windows intact.
- Decode matrix sample pairs directly into numbers, avoiding per-sample
  `RawMessage` allocations and repeated JSON decoding. Preallocate sample arrays
  and merge responses as they arrive.
- Discover metrics endpoints from one service list and at most one ingress list.
  All matching endpoints are deduplicated and offered for selection when there
  are alternatives. Previously discovery could issue up to 36 lists.
- Add phase timings to verbose CLI logs.

These collection optimizations preserve raw scrape timestamps, values, lifetime
boundaries, scrape-source selection, and analyzer policy. The subsequent one-hour
history default and rollout-fallback behavior are described in the README. Response size still depends on scrape frequency
and source multiplicity; the pod/time budget is not a byte-size guarantee.

## Measurements

Measured before the rollout-fallback change on 2026-09-22 using the current development GKE context, with only
read-only Kubernetes and Prometheus requests. The target was the cert-manager
controller deployment, retaining 14 days and three releases with the default
10 workers. It had 27 live/historical pods selected for collection.

| Measurement | Before | After |
| --- | ---: | ---: |
| Complete CLI run | 29.43 s | 5.90 s |
| Usage collection, instrumented probe | 19.51 s | 1.39 s |
| Usage requests | 112 | 62 |
| Retained CPU + memory samples | 39,712 | 39,712 |
| Recommendation analysis | 10.6 ms | 9.6 ms |

Recommendations, severities, and recommendation notes matched. These were
successive live runs, so sample timestamps and evidence windows advanced; this
is not a claim that entire reports were byte-identical. Results will vary with
network latency, workload count, history, and Prometheus load. The measured
end-to-end improvement was about 5×; collection improved about 14×.

The same CLI comparison can be reproduced with separately built before/after
binaries:

```sh
/usr/bin/time -p ./bin/kedr simple \
  --namespace cert-manager --resource Deployment \
  --selector app.kubernetes.io/component=controller \
  --history-duration-hours 336 \
  --formatter json --verbose --logtostderr > /tmp/kedr-report.json
```

Local benchmarks on an Apple M1 Pro with Go 1.27, three runs per version:

| Benchmark | Before | After |
| --- | ---: | ---: |
| Decode 80,640 native samples | 46.5–47.2 ms | 16.6–17.2 ms |
| Decode allocation volume | 27.6 MB/op | 8.4 MB/op |
| Decode allocations | about 484,000/op | about 40/op |
| One-pod history, simulated 1 ms round trips | 131–140 ms | 2.39–2.45 ms |

The latency benchmark isolates scheduling and request count using an empty
response; it does not simulate server query execution or large transfers.
Tests cover concurrency across workloads, response budgets, gap-free native
windows, lifetime boundaries, cancellation, OOM retention, endpoint discovery,
and malformed/non-finite samples.

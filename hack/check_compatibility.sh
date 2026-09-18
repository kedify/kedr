#!/bin/bash

context='gke_kedify-initial_us-central1_kedify-cluster-dev'

./bin/kedr simple -c "$context" -f json -q > kedr.json &
kedr_pid=$!

krr simple -c "$context" -f json -q > krr.json &
krr_pid=$!

wait "$kedr_pid"
wait "$krr_pid"

canonicalize() {
    jq -S '
      # KEDR intentionally uses its own branded description.
      del(
        .description,
        .config.discovery_job_batch_size,
        .config.discovery_job_max_batches
      )

      # KRR represents these values as strings; compare them numerically.
      | .config.other_args |= with_entries(
          .value |= (
            if type == "string"
            then (tonumber? // .)
            else .
            end
          )
        )

      | .scans |= (
          map(
            .object.pods = (
              (.object.pods // [])
              | sort_by([.name, .deleted])
            )
            | .object.warnings = (
                (.object.warnings // [])
                | sort
              )
          )
          | sort_by([
              .object.name,
              (.object.cluster // ""),
              .object.namespace,
              .object.kind,
              .object.container
            ])
        )

      # Normalize equivalent JSON spellings such as 12 versus 12.0.
      | walk(
          if type == "number"
          then . + 0
          else .
          end
        )
    ' "$1"
  }

canonicalize kedr.json > kedr.canonical.json
canonicalize krr.json  > krr.canonical.json

diff kedr.canonical.json krr.canonical.json

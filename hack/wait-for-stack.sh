#!/usr/bin/env bash
# Waits until the local stack has data from every team, then loads the demo
# dashboard, so reports have something to compare against. Safe to run repeatedly.
set -euo pipefail

mimir=${MIMIR_URL:-http://localhost:9009}
grafana=${GRAFANA_URL:-http://localhost:9000}
timeout=${STACK_TIMEOUT_SECONDS:-300}
here=$(cd "$(dirname "$0")" && pwd)

# series prints how many active series a job has, or 0 if Mimir can't say yet.
series() {
  curl -fsS -G "$mimir/prometheus/api/v1/cardinality/label_values" \
    -H 'X-Scope-OrgID: demo' \
    --data-urlencode 'label_names[]=job' \
    --data-urlencode "selector={job=\"$1\"}" \
    --data-urlencode 'count_method=active' 2>/dev/null |
    jq -r '.labels[0].series_count // 0' 2>/dev/null || echo 0
}

deadline=$((SECONDS + timeout))
for team in a:500 b:2000 c:15750 d:1000; do
  job="avalanche-team-${team%%:*}"
  want="${team##*:}"
  until [ "$(series "$job")" -ge "$want" ]; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "Error: $job has $(series "$job") series after ${timeout}s, want at least $want." >&2
      echo "Check it with: (cd hack/local-stack && docker compose ps && docker compose logs mimir)" >&2
      exit 1
    fi
    sleep 5
  done
  echo "$job is sending data"
done

until curl -fsS "$grafana/api/health" >/dev/null 2>&1; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "Error: Grafana isn't answering at $grafana after ${timeout}s." >&2
    exit 1
  fi
  sleep 2
done
jq '{dashboard: ., overwrite: true}' "$here/local-stack/dashboards/team-c-demo.json" |
  curl -fsS -o /dev/null -X POST -H 'Content-Type: application/json' --data @- "$grafana/api/dashboards/db"
echo "The demo dashboard is loaded"

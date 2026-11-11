#!/usr/bin/env bash
# Deploys the operator to a throwaway kind cluster connected to the local stack, and
# runs test/stack against it. Run it with `make test-stack` or `make test-extended`,
# which start the stack first. It uses its own kubeconfig, so it can't touch your
# current kubectl context. It stops Mimir and Grafana for a while to test outages,
# and starts them again afterwards.
#
# The argument chooses the suite: stack deploys with Kustomize, and extended installs
# the Helm chart.
set -euo pipefail

suite=${1:-stack}
kind_bin=${KIND:-kind}
cluster="kardigan-test-$suite"
image=kardigan:stack-test
root=$(cd "$(dirname "$0")/.." && pwd)
export KUBECONFIG="$root/bin/$suite.kubeconfig"

cd "$root"

cleanup() {
  status=$?
  # make deploy rewrites this file with the image name.
  git checkout -- config/manager/kustomization.yaml
  if [ -n "${KEEP_CLUSTER:-}" ]; then
    echo "Keeping cluster $cluster. To look around: export KUBECONFIG=$KUBECONFIG"
  else
    "$kind_bin" delete cluster --name "$cluster"
    rm -f "$KUBECONFIG"
  fi
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$root/bin"
if ! "$kind_bin" get clusters | grep -qx "$cluster"; then
  "$kind_bin" create cluster --name "$cluster" --kubeconfig "$KUBECONFIG"
fi
"$kind_bin" get kubeconfig --name "$cluster" >"$KUBECONFIG"

# Pods reach Mimir and Grafana by container name over kind's Docker network.
for container in kardigan-mimir kardigan-grafana; do
  docker network connect kind "$container" 2>/dev/null || true
  docker inspect "$container" --format '{{json .NetworkSettings.Networks}}' | grep -q '"kind"' || {
    echo "Error: couldn't connect $container to kind's network." >&2
    exit 1
  }
done

make docker-build IMG="$image"
"$kind_bin" load docker-image "$image" --name "$cluster"

case "$suite" in
  stack)
    make deploy IMG="$image" KUSTOMIZE_OVERLAY=test/stack/overlay
    kubectl -n kardigan-system rollout status deploy/kardigan-controller-manager --timeout=2m
    ;;
  extended)
    "$HELM" install kardigan charts/kardigan --namespace kardigan-system --create-namespace \
      --values test/stack/chart-values.yaml --wait --timeout 3m
    ;;
  *)
    echo "Error: unknown suite $suite, want stack or extended." >&2
    exit 1
    ;;
esac

go test -tags="$suite" ./test/stack/ -count=1 -v -timeout=30m

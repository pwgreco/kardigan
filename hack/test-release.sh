#!/usr/bin/env bash
# Builds what a release publishes and pushes it to a throwaway local registry, then
# checks it. This covers the artefacts, not the workflow that makes them: run it with
# `make test-release`.
set -euo pipefail

registry=localhost:5001
name=kardigan-test-registry
builder=kardigan-release-test
version=0.0.0-test
tag="v$version"
image="$registry/kardigan:$tag"
root=$(cd "$(dirname "$0")/.." && pwd)
workdir=$(mktemp -d)

cd "$root"

cleanup() {
  status=$?
  git checkout -- config/manager/kustomization.yaml
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker rmi "$image" >/dev/null 2>&1 || true
  rm -rf "$workdir"
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "Error: $1" >&2
  exit 1
}

docker run -d --rm --name "$name" -p "127.0.0.1:5001:5000" "$REGISTRY_IMAGE" >/dev/null
# The default builder can't export several platforms or attestations. This one needs
# the host's network to reach the registry.
docker buildx create --name "$builder" --driver docker-container --driver-opt network=host >/dev/null

BUILDX_BUILDER="$builder" make docker-buildx IMG="$image"

echo "--- platforms"
platforms=$(docker buildx imagetools inspect "$image" --raw | jq -r '.manifests[].platform | "\(.os)/\(.architecture)"')
echo "$platforms"
for want in linux/amd64 linux/arm64; do
  grep -qx "$want" <<<"$platforms" || fail "the image has no $want"
done

echo "--- SBOM and provenance"
for platform in linux/amd64 linux/arm64; do
  packages=$(BUILDX_BUILDER="$builder" docker buildx imagetools inspect "$image" \
    --format "{{json (index .SBOM \"$platform\")}}" | jq '.SPDX.packages | length')
  echo "$platform: $packages packages in the SBOM"
  [ "$packages" -gt 0 ] || fail "$platform has an empty SBOM"
  builder_id=$(BUILDX_BUILDER="$builder" docker buildx imagetools inspect "$image" \
    --format "{{json (index .Provenance \"$platform\")}}" | jq -r '.SLSA.buildDefinition.buildType // .SLSA.builder.id // empty')
  [ -n "$builder_id" ] || fail "$platform has no provenance"
done

echo "--- contents"
docker pull -q --platform linux/amd64 "$image" >/dev/null
help=$(docker run --rm "$image" -h 2>&1) || fail "the image doesn't run"
grep -q -- '-mimir-url' <<<"$help" || fail "the image's help has no -mimir-url"
[ "$(docker inspect "$image" --format '{{.Config.User}}')" = "65532:65532" ] || fail "the image doesn't run as non-root"
container=$(docker create "$image")
docker cp "$container:/licenses" "$workdir/licenses"
docker rm "$container" >/dev/null
[ -f "$workdir/licenses/LICENSE" ] && [ -f "$workdir/licenses/NOTICE" ] || fail "the image has no licence or notice"
[ -n "$(ls "$workdir/licenses/third_party")" ] || fail "the image has no third-party licences"

echo "--- install.yaml"
make build-installer IMG="$image"
for want in "image: $image" "name: cardinalitybudgets.kardigan.paulgreco.ca" "name: cardinalityreports.kardigan.paulgreco.ca"; do
  grep -q "$want" dist/install.yaml || fail "install.yaml has no '$want'"
done

echo "--- chart"
"$HELM" package charts/kardigan --version "$version" --app-version "$tag" --destination "$workdir"
"$HELM" push "$workdir/kardigan-$version.tgz" "oci://$registry/charts" --plain-http
mkdir -p "$workdir/pulled"
"$HELM" pull "oci://$registry/charts/kardigan" --version "$version" --plain-http --destination "$workdir/pulled"
rendered=$("$HELM" template kardigan "$workdir/pulled/kardigan-$version.tgz" --set mimir.url=http://mimir:8080)
grep -q "image: \"ghcr.io/pwgreco/kardigan:$tag\"" <<<"$rendered" || fail "the chart doesn't default to its own image tag"

echo "The release artefacts are good"

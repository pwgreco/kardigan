# Build the manager binary
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
# Base images are pinned by digest, so a build can't change without a commit. Dependabot updates them.
ARG BASE_IMAGE=golang:1.26.11@sha256:7af425da11f4fc1a9308eb704369e3f6d1c44e35bac0f874a59ade3f20fdc07e
# The builder runs on the build host, even for another architecture: Go cross-compiles natively, and
# running the compiler under QEMU takes many times longer.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# TARGETARCH has no default, so the binary matches the platform being built. -trimpath keeps the
# build machine's paths out of the binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o manager cmd/main.go

# Apache-2.0 and similar licences require a binary to carry its dependencies' licences and notices.
# This fails the build if a dependency's licence can't be identified.
RUN go install github.com/google/go-licenses/v2@v2.0.1 && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go-licenses save ./cmd \
    --ignore github.com/pwgreco/kardigan --save_path licenses/third_party && \
    cp LICENSE NOTICE licenses/

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/licenses /licenses
USER 65532:65532

ENTRYPOINT ["/manager"]

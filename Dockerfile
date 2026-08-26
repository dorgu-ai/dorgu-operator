# Build the manager binary.
#
# Pinned to BUILDPLATFORM so the toolchain always runs natively on the builder
# and cross-compiles to TARGETPLATFORM via GOOS/GOARCH. Without it, a
# multi-platform build runs this whole stage under QEMU for every non-native
# arch: same output, but an emulated Go compile that takes minutes instead of
# seconds. Go cross-compiles a CGO_ENABLED=0 binary natively, so there is no
# reason to pay for emulation.
FROM --platform=${BUILDPLATFORM} golang:1.25 AS builder
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

# Build.
# TARGETOS/TARGETARCH are set by BuildKit from the platform being built, and are
# what makes this a real cross-compile rather than a host-arch build: for
# `--platform linux/amd64,linux/arm64` this stage runs twice on the native
# builder, emitting one binary per target. They are left without a default on
# purpose so a plain `docker build` still gets the host arch from BuildKit.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager ./cmd/

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

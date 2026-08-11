# Multi-stage, plain Docker rather than a Nix-built image: CI and contributors
# who are not on Nix must be able to build this too. `nix build .#default`
# remains the reproducible path for releases.
#
# The build stage pins to BUILDPLATFORM and cross-compiles, rather than running
# under QEMU emulation for each target. Go cross-compiles natively, so emulating
# a whole machine to run a compiler that does not need it is slower, needs
# binfmt registered on the builder, and buys nothing.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# Both binaries, one image. They share a source tree and a release, and a
# second image would mean a second publish pipeline shipping the same commit.
# The API Deployment overrides the entrypoint.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/ ./cmd/hecate-controller ./cmd/hecate-api

# Static, non-root, no shell: the controller needs a CA bundle for registries
# and git hosts, and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hecate-controller /usr/local/bin/hecate-controller
COPY --from=build /out/hecate-api /usr/local/bin/hecate-api
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/hecate-controller"]

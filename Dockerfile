# Multi-stage, plain Docker rather than a Nix-built image: CI and contributors
# who are not on Nix must be able to build this too. `nix build .#default`
# remains the reproducible path for releases.
#
# The build stage pins to BUILDPLATFORM and cross-compiles, rather than running
# under QEMU emulation for each target. Go cross-compiles natively, so emulating
# a whole machine to run a compiler that does not need it is slower, needs
# binfmt registered on the builder, and buys nothing.
# The UI is built here rather than copied in, because pkg/api/ui is gitignored:
# the built app is a build artifact, not source, and only a .gitkeep is
# committed so `go build ./...` works for anyone without Node. That is a sound
# split, but it means an image built without this stage embeds the placeholder
# and serves "the UI was not built into this binary" — which is what every
# published image did until this stage existed. The Go build cannot fail on it,
# because the placeholder is a valid embed.
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /ui
# Manifests first so a change to the app does not re-resolve the dependency tree.
COPY ui/package.json ui/package-lock.json ./
# --include=dev explicitly: the build needs TypeScript and Tailwind, and
# NODE_ENV=production would omit exactly those.
RUN npm ci --include=dev --no-audit --no-fund
COPY ui/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# After COPY . ., which would otherwise overwrite this with the placeholder.
# Replaced wholesale rather than merged, so a page deleted from the app does not
# survive in the binary.
COPY --from=ui /ui/out/ ./pkg/api/ui/
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

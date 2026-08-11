# Multi-stage, plain Docker rather than a Nix-built image: CI and contributors
# who are not on Nix must be able to build this too. `nix build .#default`
# remains the reproducible path for releases.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/hecate-controller ./cmd/hecate-controller

# Static, non-root, no shell: the controller needs a CA bundle for registries
# and git hosts, and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hecate-controller /usr/local/bin/hecate-controller
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/hecate-controller"]

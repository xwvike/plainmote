# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG REVISION=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath \
    -ldflags="-s -w -buildid= -X plainmote/internal/app.Version=${VERSION} -X plainmote/internal/app.Revision=${REVISION}" \
    -o /out/plainmote ./cmd/plainmote

# The command line, for every platform /cli offers, built from the same
# source and stamped with the same version as the server that serves it.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS cli

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
      os="${target%/*}"; arch="${target#*/}"; ext=""; \
      if [ "$os" = windows ]; then ext=".exe"; fi; \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      go build -buildvcs=false -trimpath \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o "/out/cli/plainmote-$os-$arch$ext" ./cmd/plainmote-cli; \
    done

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="PlainMote" \
      org.opencontainers.image.description="Stateless resource sharing service" \
      org.opencontainers.image.source="https://github.com/xwvike/plainmote" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build --chown=65532:65532 /out/plainmote /plainmote
COPY --from=cli --chown=65532:65532 /out/cli /cli
USER 65532:65532
EXPOSE 8964
STOPSIGNAL SIGTERM
ENTRYPOINT ["/plainmote"]

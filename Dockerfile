# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags='-s -w -buildid=' \
    -o /out/plainmote ./cmd/plainmote

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
USER 65532:65532
EXPOSE 8964
STOPSIGNAL SIGTERM
ENTRYPOINT ["/plainmote"]

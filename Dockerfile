FROM golang:1.25-alpine AS build

WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/plainmote ./cmd/plainmote

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/plainmote /plainmote
EXPOSE 8964
ENTRYPOINT ["/plainmote"]

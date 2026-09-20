# Un Dockerfile, tres imágenes:
#   docker build --target api   -t wgrelay-api   .
#   docker build --target node  -t wgrelay-node  .
#   docker build --target agent -t wgrelay-agent .
# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64 ...

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/AndyPecotche/wg-relay/internal/buildinfo.Version=${VERSION}" \
      -o /out/ ./cmd/... \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/wgrelay-api /usr/local/bin/wgrelay-api
COPY --from=build --chown=65532:65532 /out/data /data
ENTRYPOINT ["wgrelay-api"]

FROM gcr.io/distroless/static-debian12:nonroot AS node
COPY --from=build /out/wgrelay-node /usr/local/bin/wgrelay-node
ENTRYPOINT ["wgrelay-node"]

FROM gcr.io/distroless/static-debian12:nonroot AS agent
COPY --from=build /out/wgrelay-agent /usr/local/bin/wgrelay-agent
ENV WGRELAY_CONFIG=/etc/wgrelay/wgrelay.yml
ENTRYPOINT ["wgrelay-agent"]

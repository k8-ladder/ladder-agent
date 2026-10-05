# syntax=docker/dockerfile:1
# Builds for other platforms without emulation, such as
# docker buildx build --platform linux/amd64,linux/arm64 (k8s/deploy.sh).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
# Helm keeps its chart cache here; /tmp is an emptyDir in the Deployment.
ENV HELM_CACHE_HOME=/tmp/helm/cache HELM_CONFIG_HOME=/tmp/helm/config HELM_DATA_HOME=/tmp/helm/data
USER 65532:65532
ENTRYPOINT ["/agent"]

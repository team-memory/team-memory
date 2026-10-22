# syntax=docker/dockerfile:1
# One image for every component: `serve`, `worker`, `mirror worker`,
# `migrate` and the operator CLI are subcommands of one static binary.
#
# Base images are pinned by digest. To update, resolve the new digest and
# replace both the tag comment and the digest (docs/operations/deploy.md):
#   docker buildx imagetools inspect golang:1.27.3-bookworm --format '{{json .Manifest.Digest}}'
#   docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{json .Manifest.Digest}}'

# golang:1.27.3-bookworm
FROM --platform=$BUILDPLATFORM golang@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=0.0.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/team-memory/team-memory/internal/mcp.Version=${VERSION}" \
      -o /out/team-memory ./cmd/team-memory

# gcr.io/distroless/static-debian12:nonroot (no shell, no package manager,
# no git: the mirror uses go-git in process)
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="team-memory" \
      org.opencontainers.image.description="Self-hosted, permission-aware shared memory for AI coding agents, served over MCP" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/team-memory/team-memory" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/team-memory /team-memory
USER 65532:65532
ENV TEAM_MEMORY_LISTEN=:8080 \
    TEAM_MEMORY_METRICS_LISTEN=:9090 \
    TEAM_MEMORY_MIRROR_CACHE_DIR=/tmp/mirror
EXPOSE 8080 9090
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/team-memory", "healthcheck"]
ENTRYPOINT ["/team-memory"]
CMD ["serve"]

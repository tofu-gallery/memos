# Tofu (2026-10-25): this file is Tofu's addition. Upstream builds its image from scripts/Dockerfile after building
# the web app outside Docker (`pnpm release` in web/, see .github/workflows/build-canary-image.yml). This file builds
# the web app in its first stage, so the image builds from the repository alone. The later stages follow
# scripts/Dockerfile, and the image starts through tofu-entrypoint.sh (see TOFU.md).

# The web app, built the way upstream's CI builds it, into ../server/frontend/dist, which the server embeds.
FROM node:24-alpine AS frontend
WORKDIR /frontend-build/web
RUN npm install -g pnpm@11.0.1
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
COPY web/patches ./patches
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm release

FROM golang:1.27.3-alpine AS backend
WORKDIR /backend-build

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=frontend /frontend-build/server/frontend/dist ./server/frontend/dist

ARG VERSION=0.31.0
ARG COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build \
      -trimpath \
      -ldflags="-s -w -X github.com/usememos/memos/internal/version.Version=${VERSION} -X github.com/usememos/memos/internal/version.Commit=${COMMIT} -extldflags '-static'" \
      -tags netgo,osusergo \
      -o memos \
      ./cmd/memos

FROM alpine:3.21 AS monolithic

RUN apk add --no-cache tzdata ca-certificates su-exec && \
    addgroup -g 10001 -S nonroot && \
    adduser -u 10001 -S -G nonroot -h /var/opt/memos nonroot && \
    mkdir -p /var/opt/memos /usr/local/memos && \
    chown -R nonroot:nonroot /var/opt/memos

COPY --from=backend /backend-build/memos /usr/local/memos/memos
COPY --from=backend --chmod=755 /backend-build/scripts/entrypoint.sh /usr/local/memos/entrypoint.sh
COPY --from=backend --chmod=755 /backend-build/tofu-entrypoint.sh /usr/local/memos/tofu-entrypoint.sh

# Run as root so the entrypoints can map Tofu's settings and decide whether to drop to nonroot.
USER root

WORKDIR /var/opt/memos

# Data directory. On Tofu nothing written here outlives a restart; with DATABASE_URL set, Memos keeps its data in
# PostgreSQL and uses this folder only for uploads in progress.
VOLUME /var/opt/memos

ENV TZ="UTC" \
    MEMOS_PORT="5230"

EXPOSE 5230

ENTRYPOINT ["/usr/local/memos/tofu-entrypoint.sh", "/usr/local/memos/entrypoint.sh", "/usr/local/memos/memos"]

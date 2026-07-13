# syntax=docker/dockerfile:1

# ── Stage 1: frontend ────────────────────────────────────────────────
FROM node:22-bookworm-slim AS frontend
WORKDIR /build/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ── Stage 2: Go ──────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/k12reg ./cmd/server

# ── Stage 3: runtime ─────────────────────────────────────────────────
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs ca-certificates tini \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=gobuild /out/k12reg /usr/local/bin/k12reg
COPY --from=frontend /build/frontend/dist /app/frontend/dist
COPY scripts/sentinel_vm /app/scripts/sentinel_vm
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
    && mkdir -p /data

ENV K12_DATA_DIR=/data \
    WEB_PASSWORD=admin \
    PORT=8000 \
    K12_STATIC_DIR=/app/frontend/dist \
    K12_SENTINEL_VM=/app/scripts/sentinel_vm

EXPOSE 8000

# Entrypoint ensures $K12_DATA_DIR exists (works with empty named volumes / fresh bind mounts).
ENTRYPOINT ["tini", "--", "docker-entrypoint.sh"]

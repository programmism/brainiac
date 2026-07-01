# syntax=docker/dockerfile:1

# --- build stage ---------------------------------------------------------
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=0.0.0-docker
# Build all three binaries so the CLI and MCP server are available inside the
# running container (via `docker compose exec`) — no host Go / no exposed ports.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/programmism/brainiac/internal/core.Version=${VERSION}" \
    -o /out/ ./cmd/...

# --- runtime stage -------------------------------------------------------
# Distroless static: no shell, minimal attack surface, tiny image. The app
# health-probes itself via `/brainiac healthcheck` (no shell needed). The CLI
# (/kb) and MCP server (/brainiac-mcp) are invoked with `docker compose exec`.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/http /brainiac
COPY --from=build /out/cli /kb
COPY --from=build /out/mcp /brainiac-mcp
EXPOSE 8080
ENTRYPOINT ["/brainiac"]

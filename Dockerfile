# syntax=docker/dockerfile:1

# --- build stage ---------------------------------------------------------
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=0.0.0-docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/programmism/brainiac/internal/core.Version=${VERSION}" \
    -o /out/brainiac ./cmd/http

# --- runtime stage -------------------------------------------------------
# Distroless static: no shell, minimal attack surface, tiny image. The app
# health-probes itself via `/brainiac healthcheck` (no shell needed).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/brainiac /brainiac
EXPOSE 8080
ENTRYPOINT ["/brainiac"]

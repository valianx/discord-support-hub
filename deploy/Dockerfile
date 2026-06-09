# Multi-stage build — static binary, minimal final image (NFR-10).
# CGO is disabled so the binary is fully static and works in a scratch/distroless image.

# ── Stage 1: build ───────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache dependency download separately from source compilation.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build all three binaries. CGO_ENABLED=0 produces a static binary.
# -trimpath removes local paths from the binary for reproducibility.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/api \
      ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/worker \
      ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/migrate \
      ./cmd/migrate && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/reconciler \
      ./cmd/reconciler && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/keygen \
      ./cmd/keygen

# ── Stage 2: api image ───────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12 AS api

COPY --from=builder /out/api /api
COPY --from=builder /src/migrations /migrations

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]

# ── Stage 3: worker image ─────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12 AS worker

COPY --from=builder /out/worker /worker

USER nonroot:nonroot
ENTRYPOINT ["/worker"]

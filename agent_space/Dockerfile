# Build Stage
# The tag must satisfy the `go` directive in go.mod (1.26.5); an older toolchain
# refuses the module outright.
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code
COPY . .

# Build a statically linked binary (important for scratch/alpine images)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o main .

# Final Stage
FROM alpine:latest

WORKDIR /app

# CA certificates are required for HTTPS to CockroachDB Cloud, the managed MCP
# server, and OpenRouter. wget backs the healthcheck below.
RUN apk --no-cache add ca-certificates wget

# Run unprivileged: the service only needs to read its own binary.
RUN adduser -D -u 10001 appuser

COPY --from=builder /app/main .

USER appuser

EXPOSE 8080

# /ping needs neither the database nor MCP, so it reports process liveness
# rather than dependency health. Shell form, so PORT is read at runtime —
# container platforms inject it and the server honours it.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT:-8080}/ping" || exit 1

CMD ["./main"]

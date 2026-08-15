# build
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o main .

# runtime 
FROM alpine:latest

WORKDIR /app

RUN apk --no-cache add ca-certificates wget
RUN adduser -D -u 10001 appuser

COPY --from=builder /app/main .

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT:-8080}/ping" || exit 1

CMD ["./main"]

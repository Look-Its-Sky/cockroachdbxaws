# syntax=docker/dockerfile:1.7

# Build from the service directory: docker build .

FROM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/log-analysis ./cmd/log-analysis \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/log-analysis-migrate ./cmd/log-analysis-migrate

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S analysis \
 && adduser -u 10001 -S -D -H -G analysis analysis \
 && mkdir -p /var/lib/static-log-analysis/journal \
 && chown -R 10001:10001 /var/lib/static-log-analysis

COPY --from=build /out/log-analysis /usr/local/bin/log-analysis
COPY --from=build /out/log-analysis-migrate /usr/local/bin/log-analysis-migrate

USER 10001:10001
EXPOSE 4317 4318 9464
ENTRYPOINT ["/usr/local/bin/log-analysis"]
CMD ["all"]

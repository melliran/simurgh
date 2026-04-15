# ============================================================
# simurgh — Multi-stage Docker build
# Targets:
#   server  → thefeed-server (DNS feed server)
#   client  → thefeed-client (web UI)
# ============================================================

# ---- Stage 1: Build ----
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
ARG COMMIT=unknown
ARG DATE=unknown

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/sartoopjj/thefeed/internal/version.Version=${VERSION} \
      -X github.com/sartoopjj/thefeed/internal/version.Commit=${COMMIT} \
      -X github.com/sartoopjj/thefeed/internal/version.Date=${DATE}" \
    -o /thefeed-server ./cmd/server

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/sartoopjj/thefeed/internal/version.Version=${VERSION} \
      -X github.com/sartoopjj/thefeed/internal/version.Commit=${COMMIT} \
      -X github.com/sartoopjj/thefeed/internal/version.Date=${DATE}" \
    -o /thefeed-client ./cmd/client

# ---- Stage 2: Server runtime ----
FROM alpine:3.21 AS server

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

RUN adduser -D -u 1000 -h /data thefeed

COPY --from=builder /thefeed-server /usr/local/bin/thefeed-server

VOLUME /data
EXPOSE 5300/udp

USER thefeed
WORKDIR /data

ENTRYPOINT ["thefeed-server"]
CMD ["--data-dir", "/data", "--listen", ":5300"]

# ---- Stage 3: Client runtime ----
FROM alpine:3.21 AS client

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

RUN adduser -D -u 1000 -h /clientdata thefeed

COPY --from=builder /thefeed-client /usr/local/bin/thefeed-client

VOLUME /clientdata
EXPOSE 8080/tcp

USER thefeed
WORKDIR /clientdata

ENTRYPOINT ["thefeed-client"]
CMD ["--data-dir", "/clientdata", "--port", "8080"]

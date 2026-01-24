FROM --platform=$BUILDPLATFORM golang:1.24 AS builder
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN go env -w GOPROXY=${GOPROXY} && go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 easy \
    && mkdir -p /etc/easy-proxies \
    && chown -R easy:easy /etc/easy-proxies
WORKDIR /app
COPY --from=builder /src/easy-proxies /usr/local/bin/easy-proxies
COPY --chown=easy:easy config.example.yaml /etc/easy-proxies/config.yaml
# Pool/Hybrid mode: 2323, Management: 9090, Multi-port/Hybrid mode: 24000-24200
EXPOSE 2323 9090 24000-24200
USER easy
ENTRYPOINT ["/usr/local/bin/easy-proxies"]
CMD ["--config", "/etc/easy-proxies/config.yaml"]
FROM caddy:2-builder AS caddy-builder
RUN xcaddy build --with github.com/caddy-dns/cloudflare

FROM golang:1.26.6-alpine AS sandbox-builder
WORKDIR /src
COPY go.mod ./
COPY main.go docs.html ./
COPY cmd/container-runner ./cmd/container-runner
RUN CGO_ENABLED=0 go build -trimpath -o /out/caddy-sandbox . \
    && CGO_ENABLED=0 go build -trimpath -o /out/container-runner ./cmd/container-runner

FROM caddy:2
COPY --from=caddy-builder /usr/bin/caddy /usr/bin/caddy
COPY --from=sandbox-builder /out/caddy-sandbox /usr/local/bin/caddy-sandbox
COPY --from=sandbox-builder /out/container-runner /usr/local/bin/container-runner
ENTRYPOINT ["/usr/local/bin/container-runner"]
CMD []

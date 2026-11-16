# ---- build ----
# Latest stable Go 1.x, so every rebuild picks up Go security fixes.
FROM golang:1-trixie AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN go version \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /anchor . \
 && go version -m /anchor

# ---- run ----
# Debian 13 slim (not scratch/distroless) so Unraid's built-in Tailscale
# integration can inject itself into the container. It must also run as
# root for that, so there is deliberately no USER line.
FROM debian:trixie-slim
# upgrade: pull in Debian security fixes newer than the base image.
RUN apt-get update \
 && apt-get upgrade -y --no-install-recommends \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /anchor /usr/local/bin/anchor
# Loopback by default. Set deployment mode, public URL and listener explicitly for LAN/proxy use.
ENV ANCHOR_CONFIG_DIR=/config \
    ANCHOR_LISTEN="127.0.0.1:8080,[::1]:8080"
VOLUME /config
EXPOSE 8080
# Anchor drains HTTP work for up to 25 seconds; allow time for cancellation
# and exit before the container runtime sends SIGKILL.
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/anchor", "healthcheck"]
LABEL org.opencontainers.image.title="Anchor" \
      org.opencontainers.image.description="A private dashboard for self-hosted applications"
CMD ["/usr/local/bin/anchor"]

# Build stage: Go cross-compiles on the build machine's own CPU, so a
# multi-arch build only emulates the small apt step below.
FROM --platform=$BUILDPLATFORM golang:alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web/ web/
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/rtl-sdr-scanner .

# Runtime: rtl_fm from the rtl-sdr package, lame for MP3, usbutils for lsusb
# when debugging. Debian stable tracks security updates; rebuild to pick them up.
FROM debian:stable-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends rtl-sdr lame usbutils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/rtl-sdr-scanner /usr/local/bin/rtl-sdr-scanner

ENV CONFIG_FILE=/data/config.json \
    PORT=8073
VOLUME /data
EXPOSE 8073

# Runs as root: USB passthrough to rtl_fm needs it on most hosts.
HEALTHCHECK --interval=30s --timeout=6s --start-period=10s --retries=3 \
    CMD ["rtl-sdr-scanner", "-healthcheck"]
CMD ["rtl-sdr-scanner"]

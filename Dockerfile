# ── Build Stage ───────────────────────────────────────────────────────────────
FROM python:3.11-slim AS base

LABEL maintainer="RTL-SDR Scanner"
LABEL description="Self-hosted RTL-SDR multi-frequency scanner with web UI"

# System packages: rtl-sdr provides rtl_fm, usbutils for debugging
RUN apt-get update && apt-get install -y --no-install-recommends \
        rtl-sdr \
        librtlsdr-dev \
        usbutils \
    && rm -rf /var/lib/apt/lists/*

# ── App ───────────────────────────────────────────────────────────────────────
WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY server.py .
COPY templates/ templates/

# Persistent config lives here — mount a volume to this path
RUN mkdir -p /data

# ── Runtime ───────────────────────────────────────────────────────────────────
EXPOSE 8073

ENV CONFIG_FILE=/data/config.json \
    PORT=8073

CMD ["python", "server.py"]

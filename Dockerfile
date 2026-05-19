# ── Build Stage ───────────────────────────────────────────────────────────────
FROM python:3.11-slim AS base

LABEL maintainer="RTL-SDR Scanner"
LABEL description="Self-hosted RTL-SDR multi-frequency scanner with web UI"

# System packages:
#   rtl-sdr  — provides the rtl_fm binary the scanner shells out to
#   usbutils — `lsusb` for debugging dongle visibility
# librtlsdr-dev was dropped: we don't compile anything against it.
RUN apt-get update && apt-get install -y --no-install-recommends \
        rtl-sdr \
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
    PORT=8073 \
    PYTHONUNBUFFERED=1

# USB device passthrough requires root in many setups; switching to a non-root
# user breaks rtl_fm device access on most hosts. Leaving as root for now.

CMD ["python", "server.py"]

# RTL-SDR Scanner

A self-hosted web application for scanning multiple radio frequencies with an RTL-SDR dongle. Configure a list of frequencies, set a squelch threshold, and the scanner will automatically cycle through them - dwelling on any channel where it detects a signal and advancing when silence is detected.

Any visitor can open the page and listen live. Only the admin account can add or remove frequencies, start or stop the scanner, or change settings.

---

## Features

- **Multi-frequency auto-scan** - cycles through all configured frequencies, moves to the next after a configurable silence timeout (default 2 seconds)
- **Software squelch** - tunable dBFS threshold; stays on a channel as long as signal is present
- **Live audio streaming** - raw PCM decoded in the browser via Web Audio API, no plugins required
- **Admin / visitor model** - visitors can listen; only the admin account manages frequencies and settings
- **Real-time signal meter** - 20-segment LED-style bargraph updates at 10 Hz
- **Persistent config** - frequencies and settings survive container restarts via a Docker volume
- **Modes supported** - FM, AM, USB, LSB, RAW (anything `rtl_fm` accepts)
- **PPM correction and gain control** - configurable from the web UI

---

## Requirements

| Requirement | Notes |
|---|---|
| RTL-SDR USB dongle | Any RTL2832U-based device (NooElec, RTL-SDR Blog v3, etc.) |
| Linux host | USB device passthrough requires Linux; tested on Debian/Ubuntu |
| Docker | 20.10 or newer |
| Portainer | CE or BE, any recent version |

The dongle must be plugged in **before** the container starts. If you plug it in after, restart the container.

---

## Quick Start (Portainer)

This is the recommended deployment method. Portainer's Web Editor cannot access local host directories to build images, so you must build the image manually before deploying the stack.

### 1. Get the files

Clone the repository on your Docker host:

```bash
git clone https://github.com/NullAngst/rtl-sdr-scanner.git
cd rtl-sdr-scanner
```

Or download and extract the ZIP from the Releases page.

### 2. Build the Docker Image

Build the image locally on your host machine:

```bash
docker build -t rtl-sdr-scanner:latest .
```

### 3. Generate a secret key

```bash
openssl rand -hex 32
```

Copy the output - you will paste it into the stack configuration in the next step.

### 4. Create the stack in Portainer

1. Open Portainer and navigate to **Stacks > Add stack**
2. Give the stack a name, e.g. `rtl-sdr-scanner`
3. Choose **Web editor** and paste the contents of `docker-compose.yml` directly.
4. **Delete the `build` block** from the YAML configuration since you already built the image. Ensure your service definition uses the local image:
   ```yaml
   services:
     rtl-sdr-scanner:
       image: rtl-sdr-scanner:latest
       container_name: rtl-sdr-scanner
       # ... keep the rest of your configuration
   ```
5. Under **Environment variables**, add:

   | Variable | Value |
   |---|---|
   | `SECRET_KEY` | The hex string you generated above |
   | `HOST_PORT` | The port you want to access the UI on (default: `8073`) |

6. Click **Deploy the stack**

Portainer will pull the local image from your Docker cache and start the container.

### 5. Access the UI

Open `http://your-host-ip:8073` in any browser.

Default admin credentials:

```text
Username: admin
Password: changeme
```

On first login the UI will require you to set a new password (minimum 8 characters) before continuing. Existing installs with the legacy unsalted SHA-256 hash are automatically forced into the same flow on next startup.

---

## Manual Docker Run (no Portainer)

If you prefer to run it directly:

```bash
# Build the image
docker build -t rtl-sdr-scanner .

# Run it
docker run -d \
  --name rtl-sdr-scanner \
  -p 8073:8073 \
  -e SECRET_KEY="$(openssl rand -hex 32)" \
  -e CONFIG_FILE=/data/config.json \
  -v rtl_sdr_data:/data \
  --device /dev/bus/usb:/dev/bus/usb \
  --restart unless-stopped \
  rtl-sdr-scanner
```

If device passthrough does not work, add `--privileged` instead of the `--device` flag.

---

## First-Use Walkthrough

1. **Log in** - click LOGIN in the top-right corner and enter the admin credentials
2. **Change your password** - expand the Settings panel and set a new password
3. **Add frequencies** - click `+ ADD` above the frequency list. Enter the frequency in Hz (e.g. `162400000` for 162.400 MHz NOAA Weather), a label, and a mode
4. **Start the scanner** - click START SCAN. The display will show the current frequency and cycle automatically
5. **Enable audio** - click ENABLE AUDIO. Audio starts immediately. Adjust volume with the slider
6. **Share the URL** - any visitor who opens the page can enable audio and listen live without logging in

---

## Configuration Reference

All settings are saved to `/data/config.json` inside the container (persisted via the Docker volume). You can edit them from the Settings panel in the UI, or edit the JSON file directly and restart the container.

| Setting | Default | Description |
|---|---|---|
| `squelch_db` | `-35.0` | Signal threshold in dBFS. Signals below this level are treated as silence. Increase (e.g. `-25`) to require a stronger signal; decrease (e.g. `-45`) to be more sensitive |
| `dwell_time` | `2.0` | Seconds of continuous silence before advancing to the next frequency |
| `sample_rate` | `16000` | Audio output sample rate in Hz. Higher values use more bandwidth. `16000` is good for voice |
| `ppm` | `0` | Frequency correction in parts per million for your specific dongle. Use `rtl_test -p` to measure |
| `gain` | `auto` | RF gain in dB, or `auto` to let the dongle decide. Try values like `30`, `40`, `49.6` |

### Supported Modes

| Mode | Use case |
|---|---|
| `fm` | Broadcast FM, public safety, weather radio, MURS, FRS |
| `am` | Aircraft (airband), AM broadcast |
| `usb` | Upper sideband - ham radio, maritime, military |
| `lsb` | Lower sideband - 40m/80m/160m ham bands |
| `raw` | Raw I/Q output |

---

## Environment Variables

These are set in `docker-compose.yml` or passed with `-e` on `docker run`:

| Variable | Default | Description |
|---|---|---|
| `SECRET_KEY` | random | Signs Flask session tokens. Set a fixed value if you start using Flask sessions; currently the app uses an in-memory session table that does not survive restarts |
| `CONFIG_FILE` | `/data/config.json` | Path to the config file inside the container |
| `PORT` | `8073` | Port the server listens on inside the container |
| `ALLOWED_ORIGINS` | *(unset)* | Socket.IO CORS allowlist. Unset = same-origin only. Set to `*` for unrestricted (not recommended) or a comma-separated origin list, e.g. `https://radio.example.com,https://intra.lan` |

---

## Frequency Reference (Common US Frequencies)

| Frequency | Label | Mode |
|---|---|---|
| 162400000 | NOAA Weather (WX1) | FM |
| 162425000 | NOAA Weather (WX2) | FM |
| 162450000 | NOAA Weather (WX3) | FM |
| 156800000 | Marine Channel 16 (Distress) | FM |
| 121500000 | Aircraft Emergency | AM |
| 155340000 | Fire/EMS (varies by region) | FM |
| 460000000 | FRS/GMRS (varies) | FM |

Enter frequencies in Hz (no decimals, no dots). For example, 162.400 MHz = `162400000`.

---

## Troubleshooting

### No audio in browser

The Web Audio API requires user interaction before it can play sound - this is a browser security requirement. Click **ENABLE AUDIO** after the page loads. If you still get nothing, check that the scanner is running and a frequency is active.

### `rtl_fm: command not found`

The `rtl-sdr` package was not installed in the image. Rebuild the image after verifying your Dockerfile is intact:

```bash
docker build --no-cache -t rtl-sdr-scanner .
```

### Dongle not detected

Check that the dongle is visible to the host:

```bash
lsusb | grep -i rtl
```

You should see something like `Realtek Semiconductor Corp. RTL2838 DVB-T`. If not, try a different USB port or cable.

If the host sees it but the container does not, try switching to privileged mode in `docker-compose.yml`:

```yaml
    privileged: true
    # devices:              # comment this out when using privileged
    #   - /dev/bus/usb:/dev/bus/usb
```

### Kernel driver conflict (common on Linux)

The `dvb_usb_rtl28xxu` kernel module claims the dongle before `rtl_fm` can. Blacklist it on the host:

```bash
echo 'blacklist dvb_usb_rtl28xxu' | sudo tee /etc/modprobe.d/rtlsdr.conf
sudo modprobe -r dvb_usb_rtl28xxu
```

You only need to do this once. It persists across reboots.

### Scanner skips too fast / stays too long

Adjust **Squelch** and **Dwell Time** in the Settings panel:

- Scanner moves too fast (not staying on active channels): lower the squelch threshold (more negative value)
- Scanner won't advance (stuck on noise): raise the squelch threshold (less negative value)
- Advances too quickly after signal drops: increase dwell time
- Too slow to advance: decrease dwell time

### Sessions don't survive container restarts

This is expected - the session table is in-memory. Log in again after a restart. If you need persistent sessions across restarts, switch the app to Flask's signed-cookie sessions (uses `SECRET_KEY`) or an external store like Redis.

---

## Architecture

```text
Browser (any)
    |
    |  HTTP + WebSocket (Socket.IO)
    |
Flask + Flask-SocketIO  (server.py)
    |
    |  subprocess
    |
rtl_fm  (system binary from rtl-sdr package)
    |
    |  USB
    |
RTL-SDR Dongle
```

The scanner runs in a background thread. It spawns `rtl_fm` as a subprocess and reads raw signed 16-bit PCM from its stdout in 100ms chunks. Each chunk is measured for RMS power (via NumPy), compared to the squelch threshold, and emitted to all connected browsers as a base64-encoded binary blob via Socket.IO. The browser decodes the PCM and schedules it through the Web Audio API for gapless playback.

---

## Security Notes

- This application has no HTTPS out of the box. Put it behind a reverse proxy (nginx, Caddy, Traefik) with TLS if you expose it to the internet
- The default password is `changeme` - you are forced to change it on first login (8-character minimum)
- Passwords are stored as salted hashes via Werkzeug's `generate_password_hash`
- Login attempts are rate-limited to 8 per IP per 5 minutes
- Changing the password invalidates all other active sessions
- Session tokens are stored in `localStorage` on the client and expire after 24 hours. Tokens do not survive a container restart
- Socket.IO CORS is locked to same-origin by default; widen it via `ALLOWED_ORIGINS` if you need to embed the UI cross-origin
- There is one admin account. Multiple admin accounts are not supported

---

## License

MIT License. See `LICENSE` for details.

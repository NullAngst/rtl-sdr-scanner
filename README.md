# RTL-SDR Scanner

A self-hosted scanner for one RTL-SDR dongle: it cycles through your list of frequencies, stops on anything active, and streams what it hears to any browser as plain MP3. Anyone with the URL can listen; only the admin can change channels and settings.

rtl_fm does the radio work and lame does the encoding. This program is a small Go server that supervises the two of them and serves the page.

## Features

- Scans a channel list, holds while the squelch is open, moves on after a set dwell time
- FM, AM, USB, LSB and WBFM, set per channel
- Three squelch modes: audio level, level swing (for FM quieting), and rtl_fm's own RF squelch
- Hold, skip, and click any channel to tune straight to it and hold
- Lockout: keep a channel in the list but leave it out of the scan
- Live signal meter with the squelch threshold marked on it
- Normal MP3 stream at `/stream.mp3`, so it plays in any browser, on a phone with the screen off, or in VLC
- Six themes: CRT, Light, Dark, Verdigris, Kawaii Light, Kawaii Dark
- One-time admin password on first start, no shared default

## Honest limits

- **Each hop costs 1 to 3 seconds.** rtl_fm can only listen to one frequency per run, so moving to the next channel means stopping it and starting a new one, and the dongle takes a moment to come up. A hardware scanner hops far faster. For a list of a few dozen channels it's fine. For hundreds, it's slow.
- Audio runs 2 to 4 seconds behind live, since browsers buffer MP3 streams.
- One dongle, one admin account. Logins are kept in memory, so a restart logs you out.

## Prerequisites

- An RTL2832U dongle (RTL-SDR Blog, NooElec, or whichever)
- A Linux host with Docker, and Portainer if you deploy stacks through it
- The DVB-T kernel driver blacklisted on the host (step 1 below), or it grabs the dongle before rtl_fm can

## Install with Portainer

1. Blacklist the DVB-T driver on the host, since it claims the dongle at boot:
   ```
   printf 'blacklist dvb_usb_rtl28xxu\n' | sudo tee /etc/modprobe.d/rtlsdr-blacklist.conf
   sudo modprobe -r dvb_usb_rtl28xxu
   ```
   It sticks across reboots. If `modprobe -r` says the module is in use, unplug the dongle and run it again.
2. Plug the dongle in and check the host sees it: `lsusb | grep -i rtl`. You should see something like `Realtek Semiconductor Corp. RTL2838 DVB-T`.
3. In Portainer, go to **Stacks > Add stack**, name it `rtl-sdr-scanner`, pick **Web editor**, and paste in `docker-compose.yml`.
4. Optional: under **Environment variables** set `HOST_PORT` if 8073 is taken, and `TZ` for log timestamps.
5. Click **Deploy the stack**. It pulls `ghcr.io/nullangst/rtl-sdr-scanner:latest`, so there's nothing to build.
6. Open the container's **Logs** tab and find the block that starts with `One-time admin login`. From a shell it's:
   ```
   docker logs rtl-sdr-scanner 2>&1 | grep -A3 'One-time admin login'
   ```
7. Go to `http://your-host:8073`, click **Log in**, use `admin` and that password. It makes you set your own (8 characters minimum) before anything else works.

Until you set your own, a new one-time password is generated on every restart, so a restart before you log in never locks you out.

No Portainer? Same file works with `docker compose up -d` in the folder that holds it.

## Using it

1. Click **+ Add** and enter a frequency in MHz, like `162.400`. Pick the mode: FM for narrowband voice (public safety, weather, marine, FRS/GMRS), AM for airband, WBFM for broadcast radio.
2. Click **Start scan**.
3. Click **Listen**. Anyone who opens the page can do this, logged in or not.
4. Tune the squelch (next section) until empty channels read **SQL CLOSED** and real traffic reads **SQL OPEN**.

While it's scanning:

- **Hold** stops it moving on. **Resume** lets it go again.
- **Skip** moves on right now, even mid-transmission.
- Click a channel row to tune straight to it and hold there.
- **Lock** keeps a channel in the list but out of the scan, handy for one that's stuck on noise.
- The arrows reorder the list, which is the scan order.

### Squelch modes

Pick one under Settings. Watch the meter on an empty channel while you tune it.

| Mode | How it decides | Good for |
|---|---|---|
| Audio level | Open while the level is at or above your threshold (dBFS). The line on the meter marks it. | AM, and FM channels with a quiet noise floor. Start a few dB above what an empty channel reads. |
| Level swing | Open while the level moves by at least N dB over a second. | FM quieting, where real audio on a strong carrier is quieter than open static but more dynamic. Start at 3. |
| RF carrier | rtl_fm's own squelch (`-l`). It sends nothing while the carrier is below the level. | Dead silence between transmissions. Raise from 0 until an empty channel goes quiet. Changing it restarts rtl_fm. |

**Dwell** is how long a channel has to stay closed before the scan moves on. 2 to 4 seconds catches the reply in a back-and-forth. **Silence while squelch is closed** sends silence instead of static; turn it off while you're dialing in the threshold so you can hear what the detector hears.

### Settings

| Setting | Default | Notes |
|---|---|---|
| Squelch mode | Audio level | See above |
| Open at (dBFS) | -35 | Audio level mode, -120 to 0 |
| Swing needed (dB) | 3 | Level swing mode, 0.1 to 50 |
| RF squelch level | 0 | RF carrier mode, 0 to 1000 |
| Dwell | 2 s | 0.2 to 600 |
| Silence while closed | on | |
| Audio sample rate | 16000 | 16000 is plenty for voice; use 32000 or more for WBFM. Changing it reconnects listeners |
| Gain | auto | Or a number in dB, like 40 |
| PPM correction | 0 | Your dongle's frequency error. `rtl_test -p` measures it |
| Device | first dongle | Index or serial, only needed with more than one dongle on the host |

Everything is stored in `/data/config.json` in the volume. You can hand-edit it with the container stopped. Bad values get replaced with defaults one at a time and logged, and a file that won't parse at all gets renamed to `config.json.broken-<date>` instead of being overwritten.

## Environment variables

| Variable | Default | What it does |
|---|---|---|
| `PORT` | `8073` | Port inside the container |
| `CONFIG_FILE` | `/data/config.json` | Config path |
| `TRUST_PROXY` | unset | How many reverse proxies in front of it append to `X-Forwarded-For`. `1` for NPM, NPMPlus, nginx or Caddy alone, `2` with Cloudflare in front of that. Used for the login rate limit and secure cookies. LEAVE IT UNSET WHEN NOTHING IS IN FRONT, or anyone can fake their address past the rate limit |
| `ALLOWED_ORIGINS` | unset | Extra origins allowed to send admin requests, comma separated. Only needed if your proxy rewrites the Host header |
| `MAX_LISTENERS` | `25` | Concurrent audio streams |
| `RTL_FM`, `LAME` | `rtl_fm`, `lame` | Paths, if they're not on PATH |
| `TZ` | UTC | Time zone for log timestamps |

## Behind a reverse proxy

Point the proxy at port 8073 like any other site and turn on TLS there. Set `TRUST_PROXY=1` in the stack. The server sends `X-Accel-Buffering: no` on the audio and live-update streams, so nginx and NPM pass them through without buffering. Websockets support isn't needed; it doesn't use websockets.

## Running without Docker

Each release has static binaries for amd64, arm64 and armv7. I use apt, so in my case:

1. `sudo apt install rtl-sdr lame`, or whichever package manager your distro uses.
2. Download `rtl-sdr-scanner-linux-amd64` (or the one for your CPU) from Releases and `chmod +x` it.
3. Run it: `CONFIG_FILE=$HOME/.config/rtl-sdr-scanner.json ./rtl-sdr-scanner-linux-amd64`
4. Grab the one-time password from its output and log in at `http://localhost:8073`.

Your user needs access to the dongle. The rtl-sdr package's udev rules usually handle that; if not, run it as root or add yourself to `plugdev`.

## Building the image yourself

```
git clone https://github.com/NullAngst/rtl-sdr-scanner.git
cd rtl-sdr-scanner
docker build -t rtl-sdr-scanner .
```

Then change the `image:` line in `docker-compose.yml` to `image: rtl-sdr-scanner`.

## Making a release

1. Go to the repo's **Releases** page and click **Draft a new release**.
2. Under **Choose a tag**, type a new tag like `v2.0.1` and pick **Create new tag on publish**.
3. Fill in the notes and click **Publish release**.
4. Go to **Actions**, click the **build** workflow, then **Run workflow**.
5. Under **Use workflow from**, pick the tag you just made, since the workflow builds from whatever you pick there. Click **Run workflow**.
6. It runs the checks, attaches static binaries and `SHA256SUMS` to that release, and pushes the image to `ghcr.io/nullangst/rtl-sdr-scanner` tagged `latest` and the version.

The first time, the GHCR package starts out private. Go to your profile's **Packages**, open `rtl-sdr-scanner`, **Package settings**, and set it to public, or Portainer can't pull it without a login.

If you run the workflow on `main` instead of a tag, or on a tag with no release, it stops with a message saying which. Running it again on the same tag replaces the files.

## Upgrading from the Python version

Your channels and settings carry over: on first start it reads the old `config.json` and converts it. Two things don't:

- **The admin password.** The old one was a Werkzeug scrypt hash that this version can't check, so you get a new one-time password in the log. Log in with that and set yours again.
- **RAW mode** is gone (it was never listenable). Those channels come over as FM.

"Mute dead air" was off by default before, so it's off after the upgrade. Frequencies are entered in MHz now; typing a plain Hz value like `162400000` still works.

Delete these from the repo, since nothing uses them any more: `server.py`, `requirements.txt`, `templates/`, `static/`.

## Troubleshooting

**"rtl_fm exited without producing audio" or "usb_claim_interface error -6."** Something else has the dongle. Usually the DVB-T driver (redo step 1 of the install), sometimes another container or program running rtl_fm, rtl_tcp or a decoder. After 5 failures in a row the scan stops and shows the error.

**The host sees the dongle but the container doesn't.** Restart the container after plugging it in. If that doesn't help, swap `devices:` for `privileged: true` in the stack.

**It never stops on anything.** The squelch is too tight. Turn off "Silence while squelch is closed", listen to a busy channel, and lower the threshold (Audio level) or the swing (Level swing) until it opens.

**It never moves on.** The squelch is too loose, usually loud static on an empty FM channel holding Audio level open. Raise the threshold, or switch to Level swing or RF carrier.

**Listen says "Stream dropped, reconnecting".** Normal for a moment after a sample rate change. If it keeps happening, the client is too slow for the stream and the server cut it loose; it reconnects at live.

**The page says it lost the connection.** The live updates go through `/api/events`. If you're behind a proxy, check it isn't buffering responses.

## How it works

```text
browser  <-- MP3 (/stream.mp3) and live state (/api/events) --  rtl-sdr-scanner
                                                                  |        |
                                                              PCM in    PCM out
                                                                  |        |
                                                               rtl_fm    lame
                                                                  |
                                                            RTL-SDR dongle
```

One goroutine owns rtl_fm, so there's never a second one fighting over the dongle. It reads rtl_fm's PCM in 100 ms blocks, measures each one, decides whether the squelch is open, and passes the audio to lame. lame runs the whole time the server does; between hops it gets silence, so listeners never see the stream end. The page gets state and meter updates over Server-Sent Events.

rtl_fm's demodulation rate is always a whole multiple of the output rate, since its resampler uses integer division and anything else costs level and adds distortion. AM, USB and LSB get rtl_fm's DC filter, and the level meter removes DC too, so an AM carrier with no audio on it doesn't read as signal.

## Security notes

- No HTTPS of its own. Put it behind a reverse proxy with TLS if it faces the internet.
- No shared default password. The one-time password is in the container log, so anyone who can read your logs can read it. Change it right away. Until you do, the server refuses every admin action except the password change.
- Passwords are stored as PBKDF2-SHA256 hashes, 600,000 rounds.
- Login is limited to 8 tries per address per 5 minutes.
- The session cookie is HttpOnly and SameSite=Strict, and admin requests from other sites are refused. Changing the password logs out every other session.
- The page loads nothing from outside: fonts and scripts are served by the app itself.
- The container runs as root, since USB passthrough to rtl_fm needs it on most hosts.

## License

MIT. See `LICENSE`. The VT323 and Share Tech Mono fonts in `web/fonts/` are under the SIL Open Font License, included as `web/fonts/OFL.txt`.

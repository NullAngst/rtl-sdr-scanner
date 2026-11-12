# Changes

2026.11.12 - Backend overhaul / UX overhaul

* Rewrote the server in Go as one static binary with no third-party modules, replacing the Python, Flask, Flask-SocketIO and NumPy stack
* rtl_fm is now run by a single supervisor goroutine that owns the process, so two can never fight over the dongle, and a dead rtl_fm can no longer leave the page stuck on SCANNING
* Audio is now a normal MP3 stream at /stream.mp3 encoded by lame, replacing base64 PCM over Socket.IO into a hand-built Web Audio player. Plays on phones with the screen off and in VLC
* lame runs continuously and gets silence between hops, so listeners keep one unbroken stream
* New listeners get the last two seconds of audio up front, so Listen starts much sooner
* Live state and the signal meter go over Server-Sent Events; Socket.IO and the vendored client are gone
* Channels have stable IDs instead of list positions, fixing edits and deletes landing on the wrong channel while the list changed
* Added: tune by clicking a channel (tunes and holds), lockout per channel, reorder, edit, and the squelch threshold drawn on the meter
* Added WBFM to the UI; removed RAW mode (it was never listenable). Old RAW channels become FM
* Frequencies are entered in MHz; plain Hz values still work
* AM, USB and LSB now use rtl_fm's DC filter, and the level meter removes DC, so an idle AM carrier no longer reads as signal
* Squelch level, swing and dwell changes apply live without restarting rtl_fm
* Added a device setting for hosts with more than one dongle
* Sessions are an HttpOnly, SameSite=Strict cookie instead of a token in localStorage; cross-site admin requests are refused
* Passwords are PBKDF2-SHA256 with 600,000 rounds. The one-time password is still logged on first start, and the server now refuses admin actions until it is replaced
* Strict Content-Security-Policy; fonts are served by the app instead of Google Fonts
* A config file that fails to parse is moved aside instead of overwritten; bad individual values fall back to defaults with a log line
* Reads the old Python config.json on first start and converts channels and settings
* Six themes: CRT (the original look), Light, Dark, Verdigris, Kawaii Light, Kawaii Dark
* GitHub Actions: checks on pushes and PRs; on a manual run from a release tag it attaches static binaries for amd64, arm64 and armv7 and pushes a multi-arch image to GHCR
* docker-compose.yml now pulls the published image, so Portainer no longer needs a local build
* Healthcheck uses the binary itself instead of Python

Reasons for changes: the code had grown into layers of fixes on top of two bad choices, restarting rtl_fm from tangled thread state and pushing raw PCM through Socket.IO into a custom player. Most of the 1,250-line server was patching around those. Starting over on a plain supervisor and a plain MP3 stream removed the reason for most of that code. The rtl_fm engine was kept on purpose to keep every mode, the live meter and instant skip and hold, at the cost of 1 to 3 seconds per hop.

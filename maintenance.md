# Maintenance

Do this every couple of months. Most of it is checking that nothing upstream moved; the code itself has no third-party Go modules, so there's no dependency list to chase.

## 1. Check the last build

1. Go to the repo's **Actions** tab and open the latest **build** run on `main`.
2. If it's red, open the failing step. The usual suspects:
   - **gofmt / vet / test**: something in the code, or a new Go release got stricter. Read the message, fix, push.
   - **Docker build**: `golang:alpine` or `debian:stable-slim` changed under it, or a package got renamed. See step 3.
   - An action version is gone: see step 2.
3. If it's green but old, push any small change (or run the workflow by hand on `main`) so you know it still builds today.

## 2. GitHub Actions versions

The workflow uses major versions of these actions: `actions/checkout`, `actions/setup-go`, `docker/setup-qemu-action`, `docker/setup-buildx-action`, `docker/login-action`, `docker/build-push-action`.

1. Open each one's GitHub page and look at the latest release.
2. If there's a new major version (say `@v6` became `@v7`), read its release notes for breaking changes, then bump it in `.github/workflows/build.yml`.
3. Deprecation warnings show up in the run summary as yellow annotations. Those are the ones that turn into failures later, so handle them when you see them.

Go itself needs nothing: the workflow asks for `stable` and the Dockerfile for `golang:alpine`, so both pick up the newest release on every build.

## 3. Base images and system packages

The image is `debian:stable-slim` with the `rtl-sdr`, `lame` and `usbutils` packages. Nothing is pinned, so every rebuild gets current security fixes.

1. Check that the packages still exist under those names: search them on packages.debian.org for the current stable release.
2. When Debian rolls to a new stable release, `stable-slim` moves with it. Cut a release afterwards (README, "Making a release") so the published image picks it up, then test it on your dongle before you trust it.
3. Even with no code changes, publishing a release every few months is worth it, since that's what gets security updates into `ghcr.io/nullangst/rtl-sdr-scanner:latest`.

## 4. Test on real hardware

The automated tests use no dongle, so this is the part only you can do.

1. Update the stack in Portainer (**Pull and redeploy**) or `docker compose pull && docker compose up -d`.
2. Check the log: no `WARNING: ... not found` lines, and an `audio: encoding` line.
3. Start a scan and watch it hop. Each hop logs `scan: <freq>`. Hops should take 1 to 3 seconds.
4. Open the page on a phone and on a desktop browser, press **Listen**, and make sure audio starts within a few seconds.
5. Park on a busy channel and check the meter reads and the squelch opens.

If rtl_fm misbehaves after an update (it tunes wrong, gives no audio, or prints new errors), compare against `rtl_fm` run by hand on the host. A changed `rtl_fm` flag would need a change in `rtlArgs()` in `scanner.go`.

## 5. Browser check

Load the page in current Firefox and Chrome (and Safari if you have it) and switch through all six themes. Look for anything unreadable, buttons that no longer line up, and errors in the browser console (F12).

## 6. Look over the config and logs

1. `docker logs rtl-sdr-scanner 2>&1 | grep -iE 'failed|error|rejected|broken'`. Repeated `failed login` lines from addresses you don't know mean someone is guessing; keep it off the open internet, or behind your proxy's access list.
2. If a `config.json.broken-*` file exists in the volume, the config failed to parse at some point. Check what's in it and delete it once you're sure nothing is missing.

## 7. Record it

Add an entry to `changes.md` for anything you changed, in the same format as the others.

## Where things live

| File | What's in it |
|---|---|
| `main.go` | Startup, environment variables, health check |
| `config.go` | Config format, validation, migration from the Python version, saving |
| `auth.go` | Password hashing, sessions, login rate limit, proxy and origin checks |
| `scanner.go` | The scan loop and rtl_fm handling |
| `squelch.go` | Level measurement and the three squelch modes |
| `audio.go` | lame, the silence filler, and the listener fan-out |
| `events.go` | Live updates to the page (Server-Sent Events) |
| `api.go` | HTTP routes |
| `web/` | The page: `index.html`, `app.js`, `style.css`, fonts |
| `scanner_test.go` | Tests, run with `go test ./...` |

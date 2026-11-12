// rtl-sdr-scanner scans a list of frequencies with an RTL-SDR dongle and
// streams what it hears to any browser as MP3. rtl_fm does the radio work and
// lame does the encoding; this program supervises both and serves the page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in the slim image, which has no zoneinfo
)

var version = "dev"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def, lo, hi int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "true", "yes", "on":
		if lo <= 1 && 1 <= hi {
			return 1
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		log.Printf("config: %s=%q is not a number from %d to %d, using %d", key, v, lo, hi, def)
		return def
	}
	return n
}

func main() {
	health := flag.Bool("healthcheck", false, "check the local server and exit 0 if it answers")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime)

	port := envInt("PORT", 8073, 1, 65535)
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *health {
		os.Exit(healthcheck(port))
	}

	var origins []string
	for _, o := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, strings.TrimRight(o, "/"))
		}
	}
	netCfg := Net{TrustHops: envInt("TRUST_PROXY", 0, 0, 10), Origins: origins}
	rtlBin := env("RTL_FM", "rtl_fm")
	lameBin := env("LAME", "lame")

	log.Printf("rtl-sdr-scanner %s", version)
	for _, bin := range []string{rtlBin, lameBin} {
		if _, err := exec.LookPath(bin); err != nil {
			log.Printf("WARNING: %s not found. Install it or the scanner will not work", bin)
		}
	}

	store := loadStore(env("CONFIG_FILE", "/data/config.json"))
	ensureAdminPassword(store)
	cfg := store.Snapshot()

	audio := newAudio(lameBin, cfg.Settings.SampleRate, envInt("MAX_LISTENERS", 25, 1, 1000))
	scan := newScanner(store, audio, rtlBin)
	srv := &Server{
		store:    store,
		scan:     scan,
		audio:    audio,
		hub:      newHub(),
		sessions: newSessions(),
		limiter:  newRateLimiter(),
		net:      netCfg,
		version:  version,
	}
	audio.onChange = srv.markDirty
	scan.onChange = srv.markDirty
	scan.onLevel = func(db float64, open bool) {
		srv.hub.Publish("level", map[string]any{"db": round1(db), "open": open})
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() { scan.Run(ctx); close(done) }()
	go audio.Run(ctx)
	go srv.publishLoop(ctx)

	addr := ":" + strconv.Itoa(port)
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Cancel open streams on shutdown instead of waiting them out.
		BaseContext: func(net.Listener) context.Context { return ctx },
		// No WriteTimeout: the audio and event streams stay open for hours.
	}
	go func() {
		log.Printf("listening on %s", displayAddr(addr))
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func healthcheck(port int) int {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

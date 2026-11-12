package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store    *Store
	scan     *Scanner
	audio    *Audio
	hub      *Hub
	sessions *Sessions
	limiter  *RateLimiter
	net      Net
	version  string
	dirty    atomic.Bool
}

// PublicState is everything a visitor's page needs. Nothing secret in here.
type PublicState struct {
	Status
	Listeners   int       `json:"listeners"`
	Channels    []Channel `json:"channels"`
	SquelchMode string    `json:"squelch_mode"`
	SquelchDB   float64   `json:"squelch_db"`
	Version     string    `json:"version"`
}

func (s *Server) state() PublicState {
	cfg := s.store.Snapshot()
	st := s.scan.Status()
	st.Level = round1(st.Level)
	return PublicState{
		Status:      st,
		Listeners:   s.audio.Count(),
		Channels:    cfg.Channels,
		SquelchMode: cfg.Settings.SquelchMode,
		SquelchDB:   cfg.Settings.SquelchDB,
		Version:     s.version,
	}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func (s *Server) markDirty() { s.dirty.Store(true) }

// publishLoop coalesces state changes into at most five pushes a second.
func (s *Server) publishLoop(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.dirty.Swap(false) {
				s.hub.Publish("state", s.state())
			}
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "index.html")
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(files)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /stream.mp3", s.handleStream)
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		s.hub.Serve(w, r, sseFrame("state", s.state()))
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.state()) })

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/password", s.admin(s.handlePassword, true))

	mux.HandleFunc("POST /api/scanner/{action}", s.admin(s.handleScanner, false))
	mux.HandleFunc("POST /api/channels", s.admin(s.handleAddChannel, false))
	mux.HandleFunc("PUT /api/channels/{id}", s.admin(s.handleEditChannel, false))
	mux.HandleFunc("DELETE /api/channels/{id}", s.admin(s.handleDeleteChannel, false))
	mux.HandleFunc("POST /api/channels/{id}/move", s.admin(s.handleMoveChannel, false))
	mux.HandleFunc("GET /api/settings", s.admin(s.handleGetSettings, false))
	mux.HandleFunc("PUT /api/settings", s.admin(s.handlePutSettings, false))

	return s.guard(mux)
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// guard adds security headers and refuses cross-site requests that change
// anything. That covers the login form too.
func (s *Server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if err := s.net.SameOrigin(r); err != nil {
				writeErr(w, http.StatusForbidden, err.Error())
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("request body is not valid JSON")
	}
	return nil
}

func (s *Server) token(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// admin wraps a handler that needs a login. While the one-time password is
// still in use, only the password change goes through.
func (s *Server) admin(h http.HandlerFunc, allowPending bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.Valid(s.token(r)) {
			writeErr(w, http.StatusUnauthorized, "log in first")
			return
		}
		if !allowPending && s.store.Snapshot().OneTimePending {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "set your own password first", "must_change": true})
			return
		}
		h(w, r)
	}
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    val,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.net.IsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.net.ClientIP(r)
	if !s.limiter.Allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a few minutes")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.store.Snapshot()
	userOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(cfg.AdminUser)) == 1
	passOK := checkPassword(cfg.AdminHash, body.Password) // always run it, so timing says nothing about the username
	if !userOK || !passOK {
		log.Printf("auth: failed login from %s", ip)
		writeErr(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	log.Printf("auth: admin logged in from %s", ip)
	s.setCookie(w, r, s.sessions.Create(), int(sessionTTL.Seconds()))
	writeJSON(w, 200, map[string]any{"admin": true, "must_change": cfg.OneTimePending})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Drop(s.token(r))
	s.setCookie(w, r, "", -1)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ok := s.sessions.Valid(s.token(r))
	writeJSON(w, 200, map[string]any{"admin": ok, "must_change": ok && s.store.Snapshot().OneTimePending})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if n := len([]rune(body.Password)); n < minPassword || len(body.Password) > 512 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	hash, err := hashPassword(body.Password)
	if err != nil {
		writeErr(w, 500, "could not hash password")
		return
	}
	if err := s.store.Update(func(c *Config) error {
		c.AdminHash, c.OneTimePending = hash, false
		return nil
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.sessions.DropOthers(s.token(r))
	log.Print("auth: admin password changed, other sessions logged out")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) send(r *http.Request, c command) error {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	err := s.scan.Send(ctx, c)
	s.markDirty()
	return err
}

func (s *Server) handleScanner(w http.ResponseWriter, r *http.Request) {
	var c command
	switch r.PathValue("action") {
	case "start":
		c.kind = cmdStart
	case "stop":
		c.kind = cmdStop
	case "skip":
		c.kind = cmdSkip
	case "hold":
		c.kind = cmdHold
	case "tune":
		var body struct {
			ID string `json:"id"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if !s.channelExists(body.ID) {
			writeErr(w, http.StatusNotFound, "no such channel")
			return
		}
		c.kind, c.id = cmdTune, body.ID
	default:
		writeErr(w, http.StatusNotFound, "unknown action")
		return
	}
	if err := s.send(r, c); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, 200, s.scan.Status())
}

func (s *Server) channelExists(id string) bool {
	for _, c := range s.store.Snapshot().Channels {
		if c.ID == id {
			return true
		}
	}
	return false
}

type channelBody struct {
	Freq    int64  `json:"freq"`
	Label   string `json:"label"`
	Mode    string `json:"mode"`
	Lockout bool   `json:"lockout"`
}

// channelsChanged pushes the new list to pages and tells the scanner.
func (s *Server) channelsChanged(r *http.Request) {
	_ = s.send(r, command{kind: cmdReload})
}

func (s *Server) handleAddChannel(w http.ResponseWriter, r *http.Request) {
	var b channelBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch, err := cleanChannel(Channel{Freq: b.Freq, Label: b.Label, Mode: b.Mode, Lockout: b.Lockout})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Update(func(c *Config) error {
		if len(c.Channels) >= 500 {
			return errors.New("500 channels is the limit")
		}
		c.Channels = append(c.Channels, ch)
		return nil
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.channelsChanged(r)
	writeJSON(w, http.StatusCreated, ch)
}

var errNoChannel = errors.New("no such channel")

func (s *Server) handleEditChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var b channelBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch, err := cleanChannel(Channel{ID: id, Freq: b.Freq, Label: b.Label, Mode: b.Mode, Lockout: b.Lockout})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	err = s.store.Update(func(c *Config) error {
		for i := range c.Channels {
			if c.Channels[i].ID == id {
				c.Channels[i] = ch
				return nil
			}
		}
		return errNoChannel
	})
	if errors.Is(err, errNoChannel) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.channelsChanged(r)
	writeJSON(w, 200, ch)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(c *Config) error {
		for i := range c.Channels {
			if c.Channels[i].ID == id {
				c.Channels = append(c.Channels[:i], c.Channels[i+1:]...)
				return nil
			}
		}
		return errNoChannel
	})
	if errors.Is(err, errNoChannel) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.channelsChanged(r)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMoveChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var b struct {
		Dir int `json:"dir"`
	}
	if err := readJSON(r, &b); err != nil || (b.Dir != -1 && b.Dir != 1) {
		writeErr(w, http.StatusBadRequest, "dir must be -1 or 1")
		return
	}
	err := s.store.Update(func(c *Config) error {
		for i := range c.Channels {
			if c.Channels[i].ID == id {
				j := i + b.Dir
				if j < 0 || j >= len(c.Channels) {
					return nil
				}
				c.Channels[i], c.Channels[j] = c.Channels[j], c.Channels[i]
				return nil
			}
		}
		return errNoChannel
	})
	if errors.Is(err, errNoChannel) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.channelsChanged(r)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.store.Snapshot().Settings)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	old := s.store.Snapshot().Settings
	next := old
	if err := readJSON(r, &next); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	next.normalize()
	if err := next.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Update(func(c *Config) error { c.Settings = next; return nil }); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audio.SetRate(next.SampleRate)
	_ = s.send(r, command{kind: cmdReload, radio: radioChanged(old, next)})
	writeJSON(w, 200, next)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	l, err := s.audio.Subscribe()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "listener limit reached, try again later")
		return
	}
	defer s.audio.Unsubscribe(l)
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-l.C:
			if !ok {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := w.Write(b); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// listen address helper for logs.
func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}

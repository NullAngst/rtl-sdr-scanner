package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iter    = 600_000
	sessionCookie = "sdr_session"
	sessionTTL    = 24 * time.Hour
	loginWindow   = 5 * time.Minute
	loginMax      = 8
	minPassword   = 8
)

// hashPassword returns "pbkdf2-sha256$<iter>$<salt>$<hash>", base64 raw std.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func validHash(stored string) bool {
	parts := strings.Split(stored, "$")
	return len(parts) == 4 && parts[0] == "pbkdf2-sha256"
}

// ensureAdminPassword sets a random one-time password when there is no usable
// one. It is regenerated on every start until the admin picks their own, so a
// restart before the first login never locks anyone out.
func ensureAdminPassword(st *Store) {
	cfg := st.Snapshot()
	reason := ""
	switch {
	case cfg.AdminHash == "":
		reason = "no admin password set"
	case !validHash(cfg.AdminHash):
		reason = "stored password hash is from the old version or unreadable"
	case cfg.OneTimePending:
		reason = "one-time password not replaced yet"
	default:
		return
	}
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("auth: %v", err)
	}
	pw := base64.RawURLEncoding.EncodeToString(b)
	hash, err := hashPassword(pw)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	if err := st.Update(func(c *Config) error {
		c.AdminHash = hash
		c.OneTimePending = true
		return nil
	}); err != nil {
		log.Printf("auth: could not save one-time password, it only lasts until restart: %v", err)
		st.mu.Lock()
		st.cfg.AdminHash, st.cfg.OneTimePending = hash, true
		st.mu.Unlock()
	}
	bar := strings.Repeat("=", 60)
	log.Print(bar)
	log.Printf("One-time admin login (%s):", reason)
	log.Printf("    username: %s", cfg.AdminUser)
	log.Printf("    password: %s", pw)
	log.Print("Log in and set your own password. Until you do, a new one")
	log.Print("is generated on every restart.")
	log.Print(bar)
}

// Sessions live in memory only, so a restart logs everyone out.
type Sessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessions() *Sessions { return &Sessions{m: map[string]time.Time{}} }

func (s *Sessions) Create() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	tok := hex.EncodeToString(b)
	now := time.Now()
	s.mu.Lock()
	for k, exp := range s.m {
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[tok] = now.Add(sessionTTL)
	s.mu.Unlock()
	return tok
}

func (s *Sessions) Valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	return true
}

func (s *Sessions) Drop(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

// DropOthers logs out every session except keep. Used after a password change.
func (s *Sessions) DropOthers(keep string) {
	s.mu.Lock()
	for k := range s.m {
		if k != keep {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}

// RateLimiter allows loginMax attempts per IP per loginWindow.
type RateLimiter struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func newRateLimiter() *RateLimiter { return &RateLimiter{m: map[string][]time.Time{}} }

func (r *RateLimiter) Allow(ip string) bool {
	now := time.Now()
	cut := now.Add(-loginWindow)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) > 4096 {
		for k, v := range r.m {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(r.m, k)
			}
		}
	}
	hits := r.m[ip]
	i := 0
	for i < len(hits) && hits[i].Before(cut) {
		i++
	}
	hits = hits[i:]
	if len(hits) >= loginMax {
		r.m[ip] = hits
		return false
	}
	r.m[ip] = append(hits, now)
	return true
}

// Net holds the reverse proxy and origin settings from the environment.
type Net struct {
	TrustHops int      // proxies that append to X-Forwarded-For
	Origins   []string // extra allowed origins, scheme://host[:port]
}

// ClientIP reads X-Forwarded-For from the right, since every entry to the
// left of the ones our own proxies appended is whatever the client sent.
func (n Net) ClientIP(r *http.Request) string {
	if n.TrustHops > 0 {
		var parts []string
		for _, p := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) >= n.TrustHops {
			return parts[len(parts)-n.TrustHops]
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (n Net) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return n.TrustHops > 0 && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SameOrigin rejects cross-site requests that change state. Browsers send
// Sec-Fetch-Site or Origin on these; a request with neither is not from a
// browser page, so cookies were not attached by a third-party site anyway.
func (n Net) SameOrigin(r *http.Request) error {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		if !n.originAllowed(r) {
			return errors.New("cross-site request refused")
		}
	}
	if r.Header.Get("Origin") != "" && !n.originAllowed(r) {
		return errors.New("cross-site request refused")
	}
	return nil
}

func (n Net) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	for _, a := range n.Origins {
		if a == "*" || strings.EqualFold(a, o) {
			return true
		}
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if n.TrustHops > 0 {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && strings.EqualFold(u.Host, strings.TrimSpace(strings.Split(fh, ",")[0])) {
			return true
		}
	}
	return false
}

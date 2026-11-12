package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Modes passed straight to `rtl_fm -M`.
var validModes = map[string]bool{"fm": true, "am": true, "usb": true, "lsb": true, "wbfm": true}

// Output sample rates lame can encode as MP3.
var validRates = map[int]bool{8000: true, 11025: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true}

const (
	minFreq = 500_000
	maxFreq = 2_200_000_000
)

type Channel struct {
	ID      string `json:"id"`
	Freq    int64  `json:"freq"`
	Label   string `json:"label"`
	Mode    string `json:"mode"`
	Lockout bool   `json:"lockout"`
}

type Settings struct {
	SquelchMode string  `json:"squelch_mode"` // audio, diff or rf
	SquelchDB   float64 `json:"squelch_db"`   // audio mode: open at or above this dBFS
	DiffDB      float64 `json:"diff_db"`      // diff mode: open when the 1 s swing is at least this
	RFSquelch   int     `json:"rf_squelch"`   // rf mode: rtl_fm -l value
	Dwell       float64 `json:"dwell"`        // seconds closed before moving on
	MuteClosed  bool    `json:"mute_closed"`  // send silence while squelch is closed
	SampleRate  int     `json:"sample_rate"`
	PPM         int     `json:"ppm"`
	Gain        string  `json:"gain"`   // "auto" or dB
	Device      string  `json:"device"` // rtl_fm -d: index or serial, empty for the first dongle
}

type Config struct {
	AdminUser      string    `json:"admin_user"`
	AdminHash      string    `json:"admin_hash"`
	OneTimePending bool      `json:"one_time_pending"`
	Channels       []Channel `json:"channels"`
	Settings       Settings  `json:"settings"`
}

func defaultSettings() Settings {
	return Settings{
		SquelchMode: "audio",
		SquelchDB:   -35,
		DiffDB:      3,
		RFSquelch:   0,
		Dwell:       2,
		MuteClosed:  true,
		SampleRate:  16000,
		PPM:         0,
		Gain:        "auto",
		Device:      "",
	}
}

func defaultConfig() Config {
	return Config{AdminUser: "admin", Channels: []Channel{}, Settings: defaultSettings()}
}

// validate checks every field and returns the first problem in plain words.
func (s Settings) validate() error {
	switch s.SquelchMode {
	case "audio", "diff", "rf":
	default:
		return errors.New("squelch mode must be audio, diff or rf")
	}
	if !finite(s.SquelchDB) || s.SquelchDB < -120 || s.SquelchDB > 0 {
		return errors.New("audio squelch must be between -120 and 0 dBFS")
	}
	if !finite(s.DiffDB) || s.DiffDB < 0.1 || s.DiffDB > 50 {
		return errors.New("diff squelch must be between 0.1 and 50 dB")
	}
	if s.RFSquelch < 0 || s.RFSquelch > 1000 {
		return errors.New("RF squelch must be between 0 and 1000")
	}
	if !finite(s.Dwell) || s.Dwell < 0.2 || s.Dwell > 600 {
		return errors.New("dwell must be between 0.2 and 600 seconds")
	}
	if !validRates[s.SampleRate] {
		return errors.New("unsupported sample rate")
	}
	if s.PPM < -500 || s.PPM > 500 {
		return errors.New("PPM must be between -500 and 500")
	}
	if s.Gain != "auto" {
		g, err := strconv.ParseFloat(s.Gain, 64)
		if err != nil || !finite(g) || g < 0 || g > 60 {
			return errors.New(`gain must be "auto" or a number from 0 to 60`)
		}
	}
	if len(s.Device) > 64 || strings.ContainsFunc(s.Device, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return errors.New("device must be an index or serial with no spaces")
	}
	return nil
}

// normalize trims user input into the stored form before validation.
func (s *Settings) normalize() {
	s.SquelchMode = strings.ToLower(strings.TrimSpace(s.SquelchMode))
	s.Gain = strings.ToLower(strings.TrimSpace(s.Gain))
	if s.Gain == "" {
		s.Gain = "auto"
	}
	s.Device = strings.TrimSpace(s.Device)
}

// radioChanged reports whether rtl_fm has to be restarted to apply b.
func radioChanged(a, b Settings) bool {
	return a.SampleRate != b.SampleRate || a.PPM != b.PPM || a.Gain != b.Gain ||
		a.Device != b.Device || a.SquelchMode != b.SquelchMode ||
		(b.SquelchMode == "rf" && a.RFSquelch != b.RFSquelch)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func cleanLabel(s string, freq int64) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	if s == "" {
		s = fmt.Sprintf("%.4f MHz", float64(freq)/1e6)
	}
	return s
}

// cleanChannel validates and normalizes one channel. It keeps c.ID if it is set.
func cleanChannel(c Channel) (Channel, error) {
	if c.Freq < minFreq || c.Freq > maxFreq {
		return c, fmt.Errorf("frequency must be between %.1f and %.0f MHz", float64(minFreq)/1e6, float64(maxFreq)/1e6)
	}
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = "fm"
	}
	if !validModes[c.Mode] {
		return c, errors.New("mode must be fm, am, usb, lsb or wbfm")
	}
	c.Label = cleanLabel(c.Label, c.Freq)
	if c.ID == "" {
		c.ID = newID()
	}
	return c, nil
}

// Store holds the config in memory and writes it to disk on every change.
type Store struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

func loadStore(path string) *Store {
	s := &Store{path: path, cfg: defaultConfig()}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		var cfg Config
		cfg, err = parseConfig(raw)
		if err == nil {
			s.cfg = cfg
			return s
		}
	}
	// A config that cannot be read gets moved aside rather than silently
	// overwritten, so nothing hand-edited is lost.
	bad := fmt.Sprintf("%s.broken-%s", path, time.Now().Format("20060102-150405"))
	if rerr := os.Rename(path, bad); rerr == nil {
		log.Printf("config: %v. Moved it to %s and started from defaults", err, bad)
	} else {
		log.Printf("config: %v. Could not move it aside (%v), starting from defaults", err, rerr)
	}
	return s
}

func parseConfig(raw []byte) (Config, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Config{}, fmt.Errorf("cannot parse %s", err)
	}
	cfg := defaultConfig()
	if _, legacy := probe["frequencies"]; legacy {
		if _, current := probe["channels"]; !current {
			return migrateLegacy(probe), nil
		}
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("cannot parse %s", err)
	}
	if cfg.AdminUser == "" {
		cfg.AdminUser = "admin"
	}
	cfg.Channels = cleanChannels(cfg.Channels)
	cfg.Settings = sanitizeSettings(cfg.Settings)
	return cfg, nil
}

func cleanChannels(in []Channel) []Channel {
	out := make([]Channel, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		cc, err := cleanChannel(c)
		if err != nil {
			log.Printf("config: dropping channel %+v: %v", c, err)
			continue
		}
		if seen[cc.ID] {
			cc.ID = newID()
		}
		seen[cc.ID] = true
		out = append(out, cc)
	}
	return out
}

// sanitizeSettings swaps any invalid field for its default, one at a time,
// so one bad hand edit does not reset everything.
func sanitizeSettings(s Settings) Settings {
	s.normalize()
	if s.validate() == nil {
		return s
	}
	d := defaultSettings()
	// Check each field against an otherwise-default struct.
	try := func(name string, set func(t *Settings), reset func()) {
		t := d
		set(&t)
		if err := t.validate(); err != nil {
			log.Printf("config: %s rejected (%v), using default", name, err)
			reset()
		}
	}
	try("squelch_mode", func(t *Settings) { t.SquelchMode = s.SquelchMode }, func() { s.SquelchMode = d.SquelchMode })
	try("squelch_db", func(t *Settings) { t.SquelchDB = s.SquelchDB }, func() { s.SquelchDB = d.SquelchDB })
	try("diff_db", func(t *Settings) { t.DiffDB = s.DiffDB }, func() { s.DiffDB = d.DiffDB })
	try("rf_squelch", func(t *Settings) { t.RFSquelch = s.RFSquelch }, func() { s.RFSquelch = d.RFSquelch })
	try("dwell", func(t *Settings) { t.Dwell = s.Dwell }, func() { s.Dwell = d.Dwell })
	try("sample_rate", func(t *Settings) { t.SampleRate = s.SampleRate }, func() { s.SampleRate = d.SampleRate })
	try("ppm", func(t *Settings) { t.PPM = s.PPM }, func() { s.PPM = d.PPM })
	try("gain", func(t *Settings) { t.Gain = s.Gain }, func() { s.Gain = d.Gain })
	try("device", func(t *Settings) { t.Device = s.Device }, func() { s.Device = d.Device })
	return s
}

// migrateLegacy reads the config.json the old Python server wrote. The old
// password hash was Werkzeug scrypt, which the standard library cannot check,
// so the admin gets a fresh one-time password instead.
func migrateLegacy(p map[string]json.RawMessage) Config {
	cfg := defaultConfig()
	var user string
	if json.Unmarshal(p["admin_username"], &user) == nil && user != "" {
		cfg.AdminUser = user
	}
	var freqs []map[string]any
	_ = json.Unmarshal(p["frequencies"], &freqs)
	for _, f := range freqs {
		c := Channel{}
		switch v := f["freq"].(type) {
		case float64:
			c.Freq = int64(v)
		case string:
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			c.Freq = n
		}
		c.Label, _ = f["label"].(string)
		c.Mode, _ = f["mode"].(string)
		if strings.EqualFold(c.Mode, "raw") {
			c.Mode = "fm"
		}
		cfg.Channels = append(cfg.Channels, c)
	}
	cfg.Channels = cleanChannels(cfg.Channels)

	s := defaultSettings()
	num := func(key string, dst *float64) {
		var v float64
		if json.Unmarshal(p[key], &v) == nil {
			*dst = v
		}
	}
	integer := func(key string, dst *int) {
		var v float64
		if json.Unmarshal(p[key], &v) == nil {
			*dst = int(v)
		}
	}
	var str string
	if json.Unmarshal(p["squelch_mode"], &str) == nil {
		s.SquelchMode = str
	}
	num("squelch_db", &s.SquelchDB)
	num("diff_squelch", &s.DiffDB)
	integer("rf_squelch", &s.RFSquelch)
	num("dwell_time", &s.Dwell)
	integer("sample_rate", &s.SampleRate)
	integer("ppm", &s.PPM)
	var gain any
	if json.Unmarshal(p["gain"], &gain) == nil {
		s.Gain = fmt.Sprint(gain)
	}
	// The old UI defaulted dead-air muting to off.
	s.MuteClosed = false
	cfg.Settings = sanitizeSettings(s)
	log.Printf("config: migrated %d channels from the old Python config", len(cfg.Channels))
	return cfg
}

// Snapshot returns a copy that is safe to read without the lock.
func (s *Store) Snapshot() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneConfig(s.cfg)
}

func cloneConfig(c Config) Config {
	c.Channels = append(make([]Channel, 0, len(c.Channels)), c.Channels...)
	return c
}

// Update applies fn to a copy and keeps it only if it saves to disk.
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneConfig(s.cfg)
	if err := fn(&next); err != nil {
		return err
	}
	if err := writeAtomic(s.path, next); err != nil {
		log.Printf("config: save failed: %v", err)
		return errors.New("could not save config, check the container log")
	}
	s.cfg = next
	return nil
}

func writeAtomic(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

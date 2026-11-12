package main

import (
	"encoding/binary"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pcmSine(n int, amp float64, dc int16) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(amp*math.Sin(float64(i)*0.3)) + dc
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func TestLevelDB(t *testing.T) {
	if got := levelDB(make([]byte, 3200)); got != floorDB {
		t.Fatalf("silence: got %v", got)
	}
	// Full-scale sine is about -3 dBFS.
	if got := levelDB(pcmSine(1600, 32767, 0)); math.Abs(got+3.01) > 0.2 {
		t.Fatalf("full sine: got %v", got)
	}
	// A pure DC offset must read as silence.
	if got := levelDB(pcmSine(1600, 0, 5000)); got != floorDB {
		t.Fatalf("dc only: got %v", got)
	}
	// DC plus a tone reads as the tone alone.
	a, b := levelDB(pcmSine(1600, 1000, 0)), levelDB(pcmSine(1600, 1000, 8000))
	if math.Abs(a-b) > 0.5 {
		t.Fatalf("dc changed the level: %v vs %v", a, b)
	}
}

func TestDetector(t *testing.T) {
	s := defaultSettings()
	d := newDetector(s)
	if d.feed(-50) || !d.feed(-30) {
		t.Fatal("audio mode threshold wrong")
	}

	s.SquelchMode, s.DiffDB = "diff", 3
	d.configure(s)
	if !d.feed(-20) || !d.feed(-20) {
		t.Fatal("diff mode should be open while warming up")
	}
	for i := 0; i < 10; i++ {
		d.feed(-20)
	}
	if d.feed(-20.5) {
		t.Fatal("flat static should be closed in diff mode")
	}
	if !d.feed(-28) {
		t.Fatal("a 7 dB swing should open diff mode")
	}

	s.SquelchMode = "rf"
	d.configure(s)
	if !d.feed(-100) {
		t.Fatal("rf mode: any block means open")
	}
}

func TestDemodRates(t *testing.T) {
	cases := []struct {
		mode    string
		out, in int
	}{
		{"fm", 16000, 16000},
		{"fm", 8000, 16000},
		{"am", 44100, 44100},
		{"wbfm", 32000, 160000},
		{"wbfm", 16000, 176000},
		{"usb", 22050, 22050},
	}
	for _, c := range cases {
		in, out := demodRates(c.mode, c.out)
		if in != c.in || out != c.out || in%out != 0 {
			t.Errorf("%s @%d: got %d/%d want %d", c.mode, c.out, in, out, c.in)
		}
	}
}

func TestRtlArgs(t *testing.T) {
	s := defaultSettings()
	s.SampleRate, s.Gain, s.PPM, s.Device = 8000, "40", -3, "1"
	got := strings.Join(rtlArgs(Channel{Freq: 121500000, Mode: "am"}, s), " ")
	want := "-f 121500000 -M am -s 16000 -r 8000 -p -3 -g 40 -d 1 -E dc -"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	s = defaultSettings()
	s.SquelchMode, s.RFSquelch = "rf", 25
	got = strings.Join(rtlArgs(Channel{Freq: 162400000, Mode: "fm"}, s), " ")
	if !strings.Contains(got, "-l 25") || strings.Contains(got, "-r ") {
		t.Fatalf("rf args wrong: %s", got)
	}
}

func TestPassword(t *testing.T) {
	h, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "correct horse") || checkPassword(h, "correct horsf") {
		t.Fatal("password check wrong")
	}
	for _, bad := range []string{"", "scrypt:32768:8:1$abc$def", "pbkdf2-sha256$x$y$z", "pbkdf2-sha256$99999999999$a$b"} {
		if checkPassword(bad, "x") {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCleanChannel(t *testing.T) {
	c, err := cleanChannel(Channel{Freq: 162400000, Label: "  NOAA\x07 WX1 ", Mode: "FM"})
	if err != nil || c.Label != "NOAA WX1" || c.Mode != "fm" || c.ID == "" {
		t.Fatalf("got %+v %v", c, err)
	}
	if _, err := cleanChannel(Channel{Freq: 100, Mode: "fm"}); err == nil {
		t.Fatal("accepted 100 Hz")
	}
	if _, err := cleanChannel(Channel{Freq: 162400000, Mode: "raw"}); err == nil {
		t.Fatal("accepted raw mode")
	}
	c, _ = cleanChannel(Channel{Freq: 162400000})
	if c.Label != "162.4000 MHz" || c.Mode != "fm" {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLegacyMigration(t *testing.T) {
	old := `{"admin_username":"admin","admin_password_hash":"scrypt:32768:8:1$x$y",
	"frequencies":[{"freq":"162400000","label":"WX","mode":"fm"},{"freq":121500000,"label":"Guard","mode":"am"},
	{"freq":"junk"},{"freq":156800000,"mode":"raw"}],
	"squelch_mode":"diff","squelch_db":-40,"diff_squelch":4.5,"dwell_time":3,"sample_rate":22050,"ppm":12,"gain":40}`
	cfg, err := parseConfig([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != 3 || cfg.Channels[0].Freq != 162400000 || cfg.Channels[2].Mode != "fm" {
		t.Fatalf("channels: %+v", cfg.Channels)
	}
	s := cfg.Settings
	if s.SquelchMode != "diff" || s.DiffDB != 4.5 || s.Dwell != 3 || s.SampleRate != 22050 || s.PPM != 12 || s.Gain != "40" {
		t.Fatalf("settings: %+v", s)
	}
	if cfg.AdminHash != "" {
		t.Fatal("old hash should not carry over")
	}
}

func TestSanitizeSettings(t *testing.T) {
	s := defaultSettings()
	s.SampleRate, s.Gain, s.Dwell = 12345, "loud", 5
	got := sanitizeSettings(s)
	if got.SampleRate != 16000 || got.Gain != "auto" || got.Dwell != 5 {
		t.Fatalf("got %+v", got)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	st := loadStore(path)
	if err := st.Update(func(c *Config) error {
		c.Channels = append(c.Channels, Channel{ID: "a", Freq: 162400000, Label: "WX", Mode: "fm"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	again := loadStore(path).Snapshot()
	if len(again.Channels) != 1 || again.Channels[0].Label != "WX" {
		t.Fatalf("reload: %+v", again)
	}
	// A broken file gets moved aside, not overwritten.
	os.WriteFile(path, []byte("{nope"), 0o600)
	_ = loadStore(path)
	matches, _ := filepath.Glob(path + ".broken-*")
	if len(matches) != 1 {
		t.Fatalf("broken config not moved aside: %v", matches)
	}
}

func TestPickNext(t *testing.T) {
	chs := []Channel{{ID: "a"}, {ID: "b", Lockout: true}, {ID: "c"}}
	rs := &runState{curID: "a"}
	if id, _ := next(chs, rs); id != "c" {
		t.Fatalf("next after a: %s", id)
	}
	rs.curID = "c"
	if id, _ := next(chs, rs); id != "a" {
		t.Fatalf("next after c: %s", id)
	}
	// Current channel deleted: take whatever is in its slot now.
	rs = &runState{curID: "gone", hint: 1}
	if c, _, _ := pick(chs, rs); c.ID != "c" {
		t.Fatalf("pick after delete: %s", c.ID)
	}
	// Held on a locked-out channel keeps it.
	rs = &runState{curID: "b", held: true}
	if c, _, _ := pick(chs, rs); c.ID != "b" {
		t.Fatalf("held pick: %s", c.ID)
	}
	if _, _, ok := pick([]Channel{{ID: "x", Lockout: true}}, &runState{}); ok {
		t.Fatal("all locked out should pick nothing")
	}
	pos, total := position(chs, "c")
	if pos != 2 || total != 2 {
		t.Fatalf("position: %d/%d", pos, total)
	}
}

func TestSameOrigin(t *testing.T) {
	n := Net{}
	r := httptest.NewRequest("POST", "http://radio.lan:8073/api/login", nil)
	if n.SameOrigin(r) != nil {
		t.Fatal("no headers should pass (not a browser)")
	}
	r.Header.Set("Origin", "http://radio.lan:8073")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if n.SameOrigin(r) != nil {
		t.Fatal("same origin refused")
	}
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if n.SameOrigin(r) == nil {
		t.Fatal("cross site allowed")
	}
	n.Origins = []string{"https://evil.example"}
	if n.SameOrigin(r) != nil {
		t.Fatal("listed origin refused")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if ip := (Net{}).ClientIP(r); ip != "10.0.0.5" {
		t.Fatalf("untrusted: %s", ip)
	}
	if ip := (Net{TrustHops: 1}).ClientIP(r); ip != "203.0.113.9" {
		t.Fatalf("one hop: %s", ip)
	}
	if ip := (Net{TrustHops: 2}).ClientIP(r); ip != "6.6.6.6" {
		t.Fatalf("two hops: %s", ip)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter()
	for i := 0; i < loginMax; i++ {
		if !rl.Allow("x") {
			t.Fatal("blocked too early")
		}
	}
	if rl.Allow("x") || !rl.Allow("y") {
		t.Fatal("limit wrong")
	}
}

func TestRadioChanged(t *testing.T) {
	a := defaultSettings()
	b := a
	b.SquelchDB, b.Dwell, b.MuteClosed = -20, 5, false
	if radioChanged(a, b) {
		t.Fatal("squelch level and dwell apply live")
	}
	b.RFSquelch = 10
	if radioChanged(a, b) {
		t.Fatal("rf level only matters in rf mode")
	}
	b.SquelchMode = "rf"
	if !radioChanged(a, b) {
		t.Fatal("mode change needs a restart")
	}
	c := a
	c.SampleRate = 8000
	if !radioChanged(a, c) {
		t.Fatal("rate change needs a restart")
	}
}

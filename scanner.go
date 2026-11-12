package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	blockMS     = 100                    // analysis block length
	tuneGrace   = 3 * time.Second        // rtl_fm startup before silence can count
	noDataGap   = 400 * time.Millisecond // no bytes this long means the gate is shut
	maxFailures = 5
)

// Status is what the page shows. Safe to marshal as-is.
type Status struct {
	Running bool     `json:"running"`
	Held    bool     `json:"held"`
	Channel *Channel `json:"channel"`
	Pos     int      `json:"pos"`   // 1-based position among channels not locked out
	Total   int      `json:"total"` // channels not locked out
	Open    bool     `json:"open"`
	Level   float64  `json:"level"`
	Error   string   `json:"error"`
}

type cmdKind int

const (
	cmdStart cmdKind = iota
	cmdStop
	cmdSkip
	cmdHold
	cmdTune
	cmdReload
)

type command struct {
	kind  cmdKind
	id    string // cmdTune
	radio bool   // cmdReload: rtl_fm settings changed
	reply chan error
}

// Scanner owns rtl_fm. One goroutine (Run) does all the process handling, and
// everything else talks to it through commands, so there is never more than
// one rtl_fm fighting over the dongle.
type Scanner struct {
	store *Store
	audio *Audio
	bin   string
	cmds  chan command

	onChange func()
	onLevel  func(db float64, open bool)

	mu sync.Mutex
	st Status
}

func newScanner(store *Store, audio *Audio, bin string) *Scanner {
	return &Scanner{
		store:    store,
		audio:    audio,
		bin:      bin,
		cmds:     make(chan command, 16),
		onChange: func() {},
		onLevel:  func(float64, bool) {},
		st:       Status{Level: floorDB},
	}
}

func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.Channel != nil {
		c := *st.Channel
		st.Channel = &c
	}
	return st
}

func (s *Scanner) update(fn func(st *Status)) {
	s.mu.Lock()
	fn(&s.st)
	s.mu.Unlock()
	s.onChange()
}

// Send hands a command to the scanner and waits for it to be accepted.
func (s *Scanner) Send(ctx context.Context, c command) error {
	c.reply = make(chan error, 1)
	select {
	case s.cmds <- c:
	case <-ctx.Done():
		return errors.New("scanner is busy, try again")
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return errors.New("scanner is busy, try again")
	}
}

func reply(c command, err error) {
	if c.reply != nil {
		c.reply <- err
	}
}

// runState is the loop's own bookkeeping.
type runState struct {
	running  bool
	held     bool
	curID    string // channel being listened to
	hint     int    // its index, used when it gets deleted
	failures int
}

func activeChannels(chs []Channel) []Channel {
	out := make([]Channel, 0, len(chs))
	for _, c := range chs {
		if !c.Lockout {
			out = append(out, c)
		}
	}
	return out
}

// pick finds the channel to listen to. The current one stays if it still
// exists and is not locked out (a locked-out channel can still be held, so
// tuning straight to it works). Otherwise it takes the first active channel
// at or after the old index.
func pick(chs []Channel, rs *runState) (Channel, int, bool) {
	for i, c := range chs {
		if c.ID == rs.curID && (!c.Lockout || rs.held) {
			return c, i, true
		}
	}
	n := len(chs)
	if n == 0 {
		return Channel{}, 0, false
	}
	start := min(max(rs.hint, 0), n-1)
	for k := 0; k < n; k++ {
		i := (start + k) % n
		if !chs[i].Lockout {
			return chs[i], i, true
		}
	}
	return Channel{}, 0, false
}

// next returns the active channel after the current one.
func next(chs []Channel, rs *runState) (string, int) {
	n := len(chs)
	if n == 0 {
		return "", 0
	}
	from := rs.hint
	for i, c := range chs {
		if c.ID == rs.curID {
			from = i
			break
		}
	}
	for k := 1; k <= n; k++ {
		i := (from + k) % n
		if !chs[i].Lockout {
			return chs[i].ID, i
		}
	}
	return "", 0
}

func position(chs []Channel, id string) (pos, total int) {
	for _, c := range chs {
		if c.Lockout && c.ID != id {
			continue
		}
		total++
		if c.ID == id {
			pos = total
		}
	}
	return
}

// Run is the scanner loop. It returns when ctx ends.
func (s *Scanner) Run(ctx context.Context) {
	rs := &runState{}
	for {
		if ctx.Err() != nil {
			return
		}
		if !rs.running {
			s.idle(ctx, rs)
			continue
		}
		cfg := s.store.Snapshot()
		ch, idx, ok := pick(cfg.Channels, rs)
		if !ok {
			// Nothing to scan: everything deleted or locked out.
			s.update(func(st *Status) {
				st.Channel, st.Pos, st.Total, st.Open, st.Level = nil, 0, 0, false, floorDB
			})
			s.idle(ctx, rs)
			continue
		}
		rs.curID, rs.hint = ch.ID, idx
		pos, total := position(cfg.Channels, ch.ID)
		s.update(func(st *Status) {
			c := ch
			st.Running, st.Held, st.Channel, st.Pos, st.Total = true, rs.held, &c, pos, total
			st.Open, st.Level = false, floorDB
		})
		log.Printf("scan: %.4f MHz %s [%s]", float64(ch.Freq)/1e6, ch.Label, strings.ToUpper(ch.Mode))

		res := s.listen(ctx, rs, ch, cfg.Settings)
		switch res.kind {
		case resAdvance:
			rs.curID, rs.hint = next(s.store.Snapshot().Channels, rs)
		case resFail:
			if res.ranFor > 10*time.Second {
				rs.failures = 1
			} else {
				rs.failures++
			}
			log.Printf("scan: %s", res.err)
			s.update(func(st *Status) { st.Error = res.err })
			if rs.failures >= maxFailures {
				log.Printf("scan: rtl_fm failed %d times in a row, stopping", rs.failures)
				rs.running = false
				s.update(func(st *Status) {
					st.Running, st.Held, st.Channel, st.Pos, st.Open, st.Level = false, false, nil, 0, false, floorDB
				})
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// idle waits for a command while nothing is being listened to.
func (s *Scanner) idle(ctx context.Context, rs *runState) {
	select {
	case <-ctx.Done():
	case c := <-s.cmds:
		switch c.kind {
		case cmdStart:
			err := s.startCheck()
			if err == nil {
				*rs = runState{running: true}
				s.update(func(st *Status) { st.Running, st.Held, st.Error = true, false, "" })
			}
			reply(c, err)
		case cmdStop:
			*rs = runState{}
			s.update(func(st *Status) {
				st.Running, st.Held, st.Channel, st.Pos, st.Open, st.Level = false, false, nil, 0, false, floorDB
			})
			reply(c, nil)
		case cmdTune:
			if err := s.startCheck(); err != nil {
				reply(c, err)
				return
			}
			*rs = runState{running: true, held: true, curID: c.id}
			s.update(func(st *Status) { st.Running, st.Held, st.Error = true, true, "" })
			reply(c, nil)
		case cmdReload:
			cfg := s.store.Snapshot()
			_, total := position(cfg.Channels, "")
			s.update(func(st *Status) { st.Total = total })
			reply(c, nil)
		default:
			reply(c, errors.New("the scanner is not running"))
		}
	}
}

func (s *Scanner) startCheck() error {
	if _, err := exec.LookPath(s.bin); err != nil {
		return fmt.Errorf("rtl_fm not found (%s)", s.bin)
	}
	if len(activeChannels(s.store.Snapshot().Channels)) == 0 {
		return errors.New("no channels to scan, add one or clear a lockout")
	}
	return nil
}

type resKind int

const (
	resAdvance resKind = iota
	resRetune
	resStop
	resFail
	resDone
)

type result struct {
	kind   resKind
	err    string
	ranFor time.Duration
}

// demodRates picks the rtl_fm -s rate for a mode and the -r output rate.
// rtl_fm's resampler averages rate_in/rate_out samples using integer
// division, so a ratio that is not a whole number costs level and adds
// distortion. Every rate here is an exact multiple of the output rate.
func demodRates(mode string, out int) (int, int) {
	target := 16000
	if mode == "wbfm" {
		target = 170000
	}
	mult := max(1, int(math.Round(float64(target)/float64(out))))
	return out * mult, out
}

func rtlArgs(ch Channel, set Settings) []string {
	in, out := demodRates(ch.Mode, set.SampleRate)
	// -M first: the wbfm preset sets its own rates, and ours have to win.
	args := []string{"-f", strconv.FormatInt(ch.Freq, 10), "-M", ch.Mode, "-s", strconv.Itoa(in)}
	if in != out {
		args = append(args, "-r", strconv.Itoa(out))
	}
	args = append(args, "-p", strconv.Itoa(set.PPM))
	if set.Gain != "auto" {
		args = append(args, "-g", set.Gain)
	}
	if set.Device != "" {
		args = append(args, "-d", set.Device)
	}
	if set.SquelchMode == "rf" && set.RFSquelch > 0 {
		args = append(args, "-l", strconv.Itoa(set.RFSquelch))
	}
	switch ch.Mode {
	case "am", "usb", "lsb":
		args = append(args, "-E", "dc")
	}
	return append(args, "-")
}

// errorWords marks rtl_fm stderr lines worth showing on the page.
var errorWords = []string{"error", "failed", "no supported", "not found", "busy", "cannot", "unable", "usb_claim"}

// listen runs rtl_fm on one channel until something moves the scanner on.
func (s *Scanner) listen(ctx context.Context, rs *runState, ch Channel, set Settings) result {
	cmd := exec.Command(s.bin, rtlArgs(ch, set)...)
	log.Printf("scan: %s %s", s.bin, strings.Join(cmd.Args[1:], " "))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result{kind: resFail, err: err.Error()}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return result{kind: resFail, err: err.Error()}
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return result{kind: resFail, err: "could not start rtl_fm: " + err.Error()}
	}

	var errMu sync.Mutex
	lastErr := ""
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			log.Printf("rtl_fm: %s", line)
			low := strings.ToLower(line)
			for _, w := range errorWords {
				if strings.Contains(low, w) {
					errMu.Lock()
					lastErr = line
					errMu.Unlock()
					break
				}
			}
		}
	}()

	// quit tells the reader nobody wants the audio any more. It keeps
	// draining stdout so rtl_fm never blocks on a full pipe while it is being
	// asked to exit.
	data := make(chan []byte, 64)
	quit := make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				select {
				case data <- append([]byte(nil), buf[:n]...):
				case <-quit:
				}
			}
			if err != nil {
				break
			}
		}
		close(data)
		<-stderrDone
		exited <- cmd.Wait()
	}()

	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			close(quit)
			stopProcess(cmd, exited)
		}
	}
	defer stop()

	block := set.SampleRate * blockMS / 1000 * 2
	det := newDetector(set)
	silence := make([]byte, block)
	var (
		buf         []byte
		gotData     bool
		lastData    time.Time
		closedSince time.Time
		lastEmit    time.Time
	)
	tick := time.NewTicker(blockMS * time.Millisecond)
	defer tick.Stop()

	emit := func(db float64, open bool, force bool) {
		now := time.Now()
		s.mu.Lock()
		changed := s.st.Open != open
		s.st.Level, s.st.Open = db, open
		s.mu.Unlock()
		if force || changed || now.Sub(lastEmit) >= 90*time.Millisecond {
			lastEmit = now
			s.onLevel(db, open)
		}
	}
	dwellUp := func(now time.Time) bool {
		return !rs.held && !closedSince.IsZero() && now.Sub(closedSince) >= time.Duration(set.Dwell*float64(time.Second))
	}

	for {
		select {
		case <-ctx.Done():
			return result{kind: resDone}

		case c := <-s.cmds:
			switch c.kind {
			case cmdStart:
				reply(c, nil)
			case cmdStop:
				rs.running, rs.held = false, false
				stop()
				s.update(func(st *Status) {
					st.Running, st.Held, st.Channel, st.Pos, st.Open, st.Level = false, false, nil, 0, false, floorDB
				})
				reply(c, nil)
				return result{kind: resStop}
			case cmdSkip:
				reply(c, nil)
				log.Printf("scan: skipped %.4f MHz", float64(ch.Freq)/1e6)
				return result{kind: resAdvance}
			case cmdHold:
				rs.held = !rs.held
				closedSince = time.Time{}
				log.Printf("scan: hold %v on %.4f MHz", rs.held, float64(ch.Freq)/1e6)
				s.update(func(st *Status) { st.Held = rs.held })
				reply(c, nil)
			case cmdTune:
				rs.held = true
				reply(c, nil)
				if c.id == ch.ID {
					s.update(func(st *Status) { st.Held = true })
					continue
				}
				rs.curID = c.id
				return result{kind: resRetune}
			case cmdReload:
				reply(c, nil)
				cfg := s.store.Snapshot()
				var cur *Channel
				for i := range cfg.Channels {
					if cfg.Channels[i].ID == ch.ID {
						cur = &cfg.Channels[i]
						break
					}
				}
				if cur == nil || (cur.Lockout && !rs.held) {
					// Deleted or locked out: whatever slid into its slot is next.
					rs.curID = ""
					return result{kind: resRetune}
				}
				if c.radio || cur.Freq != ch.Freq || cur.Mode != ch.Mode {
					return result{kind: resRetune}
				}
				set = cfg.Settings
				det.configure(set)
				pos, total := position(cfg.Channels, cur.ID)
				cc := *cur
				s.update(func(st *Status) { st.Channel, st.Pos, st.Total = &cc, pos, total })
			}

		case b, ok := <-data:
			if !ok {
				data = nil // wait for exited
				continue
			}
			now := time.Now()
			gotData, lastData = true, now
			buf = append(buf, b...)
			for len(buf) >= block {
				pcm := buf[:block]
				db := levelDB(pcm)
				open := det.feed(db)
				emit(db, open, false)
				if open || !set.MuteClosed {
					s.audio.Push(set.SampleRate, pcm)
				} else {
					s.audio.Push(set.SampleRate, silence)
				}
				if open {
					closedSince = time.Time{}
				} else if closedSince.IsZero() {
					closedSince = now
				}
				buf = buf[block:]
			}
			buf = append([]byte(nil), buf...)
			if dwellUp(now) {
				log.Printf("scan: quiet for %.1fs on %.4f MHz, moving on", set.Dwell, float64(ch.Freq)/1e6)
				return result{kind: resAdvance}
			}

		case err := <-exited:
			stopped = true
			close(quit)
			errMu.Lock()
			msg := lastErr
			errMu.Unlock()
			if msg == "" {
				msg = "rtl_fm exited"
				if err != nil {
					msg += " (" + err.Error() + ")"
				}
				if !gotData {
					msg += " without producing audio. Check that the dongle is plugged in and not claimed by another program or the DVB kernel driver"
				}
			}
			return result{kind: resFail, err: msg, ranFor: time.Since(started)}

		case now := <-tick.C:
			// rtl_fm writes in bursts, so one quiet tick means nothing. A
			// sustained gap means either rtl_fm is still starting (dongle init
			// takes 1-3 s) or its own RF squelch is holding the gate shut.
			// Only the second is dead air, hence the grace period.
			ref := lastData
			if !gotData {
				if now.Sub(started) < tuneGrace {
					continue
				}
				ref = started.Add(tuneGrace - noDataGap)
			}
			if now.Sub(ref) < noDataGap {
				continue
			}
			det.reset()
			emit(floorDB, false, false)
			if closedSince.IsZero() {
				closedSince = now
			}
			if dwellUp(now) {
				log.Printf("scan: no signal for %.1fs on %.4f MHz, moving on", set.Dwell, float64(ch.Freq)/1e6)
				return result{kind: resAdvance}
			}
		}
	}
}

// stopProcess asks rtl_fm to exit and waits, so the dongle is free before the
// next one opens it.
func stopProcess(cmd *exec.Cmd, exited chan error) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(2 * time.Second):
	}
	_ = cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		log.Print("scan: rtl_fm did not exit after SIGKILL")
	}
}

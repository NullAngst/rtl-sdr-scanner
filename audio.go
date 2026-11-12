package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Audio turns the scanner's PCM into one continuous MP3 stream with lame and
// fans it out to every listener. The encoder runs for as long as the server
// does. Between hops, while rtl_fm restarts, it gets silence, so players never
// see a gap or a new stream.
type Audio struct {
	lame string
	max  int

	mu        sync.Mutex
	rate      int
	fifo      []byte // PCM waiting for the pacer
	listeners map[*Listener]struct{}
	burst     [][]byte // the last couple of seconds of MP3, sent to new listeners
	burstLen  int
	restart   chan struct{}
	onChange  func()
}

type Listener struct {
	C    chan []byte
	once sync.Once
}

func (l *Listener) close() { l.once.Do(func() { close(l.C) }) }

var errFull = errors.New("listener limit reached")

func newAudio(lame string, rate, max int) *Audio {
	return &Audio{
		lame:      lame,
		max:       max,
		rate:      rate,
		listeners: map[*Listener]struct{}{},
		restart:   make(chan struct{}, 1),
		onChange:  func() {},
	}
}

func mp3Bitrate(rate int) int {
	switch {
	case rate <= 11025:
		return 16
	case rate <= 16000:
		return 32
	case rate <= 24000:
		return 48
	default:
		return 64
	}
}

// Push queues PCM for the encoder. Audio for a sample rate the encoder is not
// running at is dropped; it only shows up for a moment around a rate change.
func (a *Audio) Push(rate int, pcm []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rate != a.rate {
		return
	}
	a.fifo = append(a.fifo, pcm...)
	// Keep at most one second queued. rtl_fm's clock is the dongle's crystal
	// and ours is the host's, so they drift apart a little over time.
	if limit := a.rate * 2; len(a.fifo) > limit {
		a.fifo = append([]byte(nil), a.fifo[len(a.fifo)-limit:]...)
	}
}

// SetRate restarts the encoder at a new rate. Listeners get disconnected,
// since an MP3 stream cannot change rate halfway, and the page reconnects.
func (a *Audio) SetRate(rate int) {
	a.mu.Lock()
	if rate == a.rate {
		a.mu.Unlock()
		return
	}
	a.rate = rate
	a.fifo = nil
	a.burst, a.burstLen = nil, 0
	a.dropListenersLocked()
	a.mu.Unlock()
	select {
	case a.restart <- struct{}{}:
	default:
	}
	a.onChange()
}

func (a *Audio) Subscribe() (*Listener, error) {
	a.mu.Lock()
	if len(a.listeners) >= a.max {
		a.mu.Unlock()
		return nil, errFull
	}
	l := &Listener{C: make(chan []byte, 256)}
	// Browsers buffer a few seconds before they start playing a stream this
	// slow. Handing them the recent past up front makes Listen start in about
	// a second, the same trick Icecast uses.
	for _, b := range a.burst {
		l.C <- b
	}
	a.listeners[l] = struct{}{}
	a.mu.Unlock()
	a.onChange()
	return l, nil
}

func (a *Audio) Unsubscribe(l *Listener) {
	a.mu.Lock()
	_, ok := a.listeners[l]
	delete(a.listeners, l)
	a.mu.Unlock()
	l.close()
	if ok {
		a.onChange()
	}
}

func (a *Audio) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.listeners)
}

func (a *Audio) dropListenersLocked() {
	for l := range a.listeners {
		delete(a.listeners, l)
		l.close()
	}
}

func (a *Audio) broadcast(b []byte) {
	a.mu.Lock()
	a.burst = append(a.burst, b)
	a.burstLen += len(b)
	limit := mp3Bitrate(a.rate) * 1000 / 8 * 2 // two seconds
	for len(a.burst) > 1 && (a.burstLen > limit || len(a.burst) > 128) {
		a.burstLen -= len(a.burst[0])
		a.burst = a.burst[1:]
	}
	dropped := false
	for l := range a.listeners {
		select {
		case l.C <- b:
		default:
			// Too far behind to ever catch up. Cut it loose; the page
			// reconnects and starts again from live.
			delete(a.listeners, l)
			l.close()
			dropped = true
		}
	}
	a.mu.Unlock()
	if dropped {
		a.onChange()
	}
}

// Run keeps lame running until ctx ends.
func (a *Audio) Run(ctx context.Context) {
	for ctx.Err() == nil {
		a.mu.Lock()
		rate := a.rate
		a.mu.Unlock()
		err := a.encode(ctx, rate)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("audio: lame stopped: %v, restarting", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (a *Audio) encode(parent context.Context, rate int) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	khz := strconv.FormatFloat(float64(rate)/1000, 'f', -1, 64)
	cmd := exec.CommandContext(ctx, a.lame,
		"-r", "-s", khz, "--bitwidth", "16", "--signed", "--little-endian",
		"-m", "m", "--cbr", "-b", strconv.Itoa(mp3Bitrate(rate)),
		"--flush", "--quiet", "-", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("audio: encoding %d Hz mono MP3 at %d kbps", rate, mp3Bitrate(rate))

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			// Distro builds of lame linked against libsndfile print this on
			// every start. It does not apply to stdin input.
			if line == "" || strings.Contains(line, "libsndfile") {
				continue
			}
			log.Printf("lame: %s", line)
		}
	}()

	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				a.broadcast(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = errors.New("encoder exited")
				}
				readDone <- err
				return
			}
		}
	}()

	pacerDone := make(chan error, 1)
	go func() { pacerDone <- a.pace(ctx, stdin, rate) }()

	var result error
	select {
	case <-ctx.Done():
	case <-a.restart:
	case result = <-readDone:
	case result = <-pacerDone:
	}
	cancel()
	stdin.Close()
	_ = cmd.Wait()
	return result
}

// pace feeds lame exactly `rate` samples per second of wall-clock time:
// queued PCM if there is any, silence if not.
func (a *Audio) pace(ctx context.Context, w io.Writer, rate int) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	var written int64
	zeros := make([]byte, rate) // half a second of silence
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		due := int64(time.Since(start).Seconds()*float64(rate)) - written
		if due <= 0 {
			continue
		}
		if max := int64(rate / 2); due > max {
			// The host stalled. Skip ahead instead of bursting.
			written += due - max
			due = max
		}
		need := int(due) * 2
		a.mu.Lock()
		take := min(need, len(a.fifo)&^1)
		out := make([]byte, 0, need)
		out = append(out, a.fifo[:take]...)
		a.fifo = a.fifo[take:]
		a.mu.Unlock()
		for len(out) < need {
			out = append(out, zeros[:min(len(zeros), need-len(out))]...)
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		written += due
	}
}

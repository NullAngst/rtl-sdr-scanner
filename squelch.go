package main

import (
	"encoding/binary"
	"math"
)

const floorDB = -100.0

// levelDB returns the RMS level of a block of signed 16-bit little-endian mono
// PCM in dBFS. The block mean is removed first, so a DC offset (an AM carrier
// with no audio on it, or a dongle with DC bias) does not read as signal.
func levelDB(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return floorDB
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
	}
	mean := sum / float64(n)
	var sq float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) - mean
		sq += v * v
	}
	rms := math.Sqrt(sq / float64(n))
	if rms < 1 {
		return floorDB
	}
	db := 20 * math.Log10(rms/32768)
	if db < floorDB {
		return floorDB
	}
	return db
}

// detector decides, one 100 ms block at a time, whether the squelch is open.
//
// audio: open while the level is at or above SquelchDB. Simple, but loud
// static on an empty FM channel reads as signal.
//
// diff: open while the level swings by at least DiffDB across the last
// second. Made for FM quieting, where a strong carrier with voice is quieter
// and more dynamic than flat open static. An absolute floor would break this
// mode, so there is none.
//
// rf: rtl_fm does the squelch itself (-l) and sends nothing while closed, so
// any block that arrives means open. Gaps are handled by the scanner.
type detector struct {
	mode   string
	sqDB   float64
	diffDB float64
	hist   [10]float64
	n      int // filled entries, up to len(hist)
	pos    int
}

func newDetector(s Settings) *detector {
	d := &detector{}
	d.configure(s)
	return d
}

func (d *detector) configure(s Settings) {
	if d.mode != s.SquelchMode {
		d.reset()
	}
	d.mode, d.sqDB, d.diffDB = s.SquelchMode, s.SquelchDB, s.DiffDB
}

func (d *detector) reset() { d.n, d.pos = 0, 0 }

func (d *detector) feed(db float64) bool {
	switch d.mode {
	case "rf":
		return true
	case "diff":
		d.hist[d.pos] = db
		d.pos = (d.pos + 1) % len(d.hist)
		if d.n < len(d.hist) {
			d.n++
		}
		// Not enough history to judge a freshly tuned channel yet. Call it
		// open, so nothing gets skipped before it can be measured.
		if d.n < 3 {
			return true
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for i := 0; i < d.n; i++ {
			lo = math.Min(lo, d.hist[i])
			hi = math.Max(hi, d.hist[i])
		}
		return hi-lo >= d.diffDB
	default:
		return db >= d.sqDB
	}
}

// Package pace works out how much a connection can carry, from how long
// frames take to be acknowledged, so that the picture can be sized to fit.
//
// On a local network a frame of any size arrives almost at once, and nothing
// here changes anything. Across the internet the sending PC's upload speed
// is usually the limit: a full-quality frame of a large screen can be a
// megabyte or two, which at 15 Mbit/s is a second each. Knowing the speed,
// the server sends a large change in pieces that each cross the link in
// about a tenth of a second, eases picture quality, and holds video to a
// bit rate the link can carry.
//
// The speed is only ever learned from frames the link visibly held up:
// ones that took longer to be acknowledged than the round trip alone
// explains. Frames that arrive as fast as they are sent say nothing about
// the link's limit, however slowly they were being sent.
package pace

import (
	"math"
	"sync"
	"time"
)

const (
	// Fast is the speed, in bits a second, from which a link is left
	// alone: full quality, as on a local network.
	Fast = 60e6

	frameTime = 120 * time.Millisecond // how long one frame may take to cross a limited link
	evidence  = 20 * time.Millisecond  // delay beyond the round trip that shows the link set the pace
	bigFrame  = 16 << 10               // frames smaller than this say little about speed
)

type frame struct {
	at    time.Time
	bytes int64
}

// Meter measures one connection. The zero value is ready to use, and all
// methods are safe to call from different goroutines.
type Meter struct {
	mu      sync.Mutex
	queue   []frame // sent, not yet acknowledged, oldest first
	prevAck time.Time

	// The quickest acknowledgement in the current half minute and the one
	// before: about the round trip, plus the client's handling of a frame.
	baseNow, basePrev time.Duration
	baseTurn          time.Time

	rate float64 // bits a second the link carries; 0 while it has shown no limit
	good int     // sizeable frames in a row that were not held up

	cap, capWant float64 // video bit rate ceiling in force, and one being considered
	capSince     time.Time
}

// Sent records a frame about to go out: when its first byte was written,
// and its size. Call it before the message the client acknowledges.
func (m *Meter) Sent(at time.Time, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) >= 32 { // acknowledgements have gone missing: start again
		m.queue = m.queue[:0]
	}
	m.queue = append(m.queue, frame{at, bytes})
}

func (m *Meter) base() time.Duration {
	switch {
	case m.baseNow == 0:
		return m.basePrev
	case m.basePrev == 0:
		return m.baseNow
	}
	return min(m.baseNow, m.basePrev)
}

// Acked records that the client acknowledged the oldest frame outstanding.
func (m *Meter) Acked(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) == 0 {
		return
	}
	f := m.queue[0]
	m.queue = m.queue[1:]
	took := now.Sub(f.at)
	if took <= 0 {
		return
	}
	if m.baseTurn.IsZero() || now.Sub(m.baseTurn) > 30*time.Second {
		m.basePrev, m.baseNow, m.baseTurn = m.baseNow, 0, now
	}
	if m.baseNow == 0 || took < m.baseNow {
		m.baseNow = took
	}
	base := m.base()
	// The frame's own journey began when the link was free for it: once
	// the frame before had arrived, or a round trip after it was sent,
	// whichever came later.
	began := f.at.Add(base)
	if m.prevAck.After(began) {
		began = m.prevAck
	}
	m.prevAck = now
	crossing := now.Sub(began)
	heldUp := took-base >= evidence

	switch {
	case f.bytes >= bigFrame && heldUp && crossing >= 15*time.Millisecond:
		sample := float64(f.bytes) * 8 / crossing.Seconds()
		switch {
		case m.rate == 0:
			m.rate = sample
		case sample < m.rate:
			m.rate += 0.5 * (sample - m.rate) // believe bad news quickly
		default:
			m.rate += 0.2 * (sample - m.rate)
		}
		m.good = 0
	case m.rate > 0 && !heldUp && f.bytes >= max(bigFrame, m.budget()/4):
		// A frame that should have taken a noticeable time did not: the
		// link is quicker than thought.
		if m.good++; m.good >= 5 {
			m.rate *= 1.25
			m.good = 0
		}
	}
	if m.rate >= Fast {
		m.rate, m.good = 0, 0
	}
}

// budget is the size a frame should stay under, in bytes; 0 for no limit.
func (m *Meter) budget() int64 {
	if m.rate == 0 {
		return 0
	}
	return int64(m.rate / 8 * frameTime.Seconds())
}

// Budget is the size one frame should stay under so that it crosses the
// link in about a tenth of a second, in bytes; 0 means no limit.
func (m *Meter) Budget() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.budget()
}

// Limited reports whether the link has shown itself to be slower than a
// local network.
func (m *Meter) Limited() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rate > 0
}

// Quality turns the picture quality the client asked for (1-100) into the
// one to use now: as asked on a quick link, easing down to a little over
// half of it on a slow one. Text and plain areas are sent exactly whatever
// the quality, so this mostly affects photographs and gradients.
func (m *Meter) Quality(asked int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rate == 0 {
		return asked
	}
	factor := min(0.55+0.45*m.rate/40e6, 1)
	return min(asked, max(30, int(math.Round(float64(asked)*factor))))
}

// VideoCap is the most the video encoder should produce, in bits a second,
// or 0 for no ceiling. It moves in steps, comes down at once and goes up
// only after the link has looked better for a few seconds, because every
// change means starting the video stream afresh.
func (m *Meter) VideoCap(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rate == 0 {
		m.cap, m.capWant = 0, 0
		return 0
	}
	// Steps of a half more each, from 250 kbit/s.
	want := 250e3 * math.Pow(1.5, math.Floor(math.Log(max(0.65*m.rate, 250e3)/250e3)/math.Log(1.5)))
	switch {
	case m.cap == 0 || want < m.cap:
		m.cap, m.capWant = want, 0
	case want > m.cap:
		if m.capWant != want {
			m.capWant, m.capSince = want, now
		} else if now.Sub(m.capSince) >= 5*time.Second {
			m.cap, m.capWant = want, 0
		}
	default:
		m.capWant = 0
	}
	return int(m.cap)
}

// Window is how many frames may be on their way at once. Two suits a local
// network. When the round trip is long, a few more keep the frame rate from
// being tied to it.
func (m *Meter) Window(interval time.Duration) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	base := m.base()
	if base < 20*time.Millisecond || interval <= 0 {
		return 2
	}
	extra := min(int32(base/interval), 3)
	if m.rate > 0 {
		extra = min(extra, 1) // frames already take a while each
	}
	return 2 + extra
}

// Info is what there is to tell the person: the link's speed in bits a
// second (0 if it has shown no limit) and the round trip.
func (m *Meter) Info() (rate float64, roundTrip time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rate, m.base()
}

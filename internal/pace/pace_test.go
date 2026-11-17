package pace

import (
	"math/rand"
	"testing"
	"time"
)

// link is a simulated connection: so many bits a second, so long a round
// trip, and a client that takes a while to draw each megabyte.
type link struct {
	bps       float64
	roundTrip time.Duration
	drawPerMB time.Duration
	free      time.Time // when its last frame finishes arriving
}

// deliver says when a frame sent at t is acknowledged.
func (l *link) deliver(t time.Time, bytes int64) time.Time {
	start := t
	if l.free.After(start) {
		start = l.free
	}
	l.free = start.Add(time.Duration(float64(bytes) * 8 / l.bps * float64(time.Second)))
	return l.free.Add(l.roundTrip + time.Duration(float64(bytes)/1e6*float64(l.drawPerMB)))
}

// run plays a session: mostly small changes, some middling, now and then a
// screenful, each sent the way the server does: at the quality the meter
// gives, and in pieces no bigger than its budget. It returns the meter and
// the longest any piece took to be acknowledged in the last third.
func run(l *link, seconds int) (*Meter, time.Duration) {
	var m Meter
	rng := rand.New(rand.NewSource(1))
	now := time.Unix(1000, 0)
	end := now.Add(time.Duration(seconds) * time.Second)
	lateFrom := now.Add(time.Duration(seconds) * time.Second * 2 / 3)
	var acks []time.Time // when each outstanding frame is acknowledged
	var sentAt []time.Time
	var worst time.Duration
	var left int64 // of a change being sent in pieces
	for now.Before(end) {
		for len(acks) > 0 && !acks[0].After(now) {
			m.Acked(acks[0])
			if sentAt[0].After(lateFrom) {
				worst = max(worst, acks[0].Sub(sentAt[0]))
			}
			acks, sentAt = acks[1:], sentAt[1:]
		}
		if int32(len(acks)) >= m.Window(33*time.Millisecond) {
			now = now.Add(2 * time.Millisecond)
			continue
		}
		if left == 0 {
			full := float64(60 << 10) // a small change at full quality
			switch n := rng.Intn(100); {
			case n < 4:
				full = 2 << 20 // a screenful
			case n < 30:
				full = 300 << 10
			}
			// Size follows quality roughly in proportion.
			left = int64(full * float64(m.Quality(80)) / 80)
		}
		bytes := left
		if b := m.Budget(); b > 0 && bytes > b {
			bytes = b
		}
		left -= bytes
		m.Sent(now, bytes)
		acks = append(acks, l.deliver(now, bytes))
		sentAt = append(sentAt, now)
		now = now.Add(33 * time.Millisecond)
	}
	return &m, worst
}

func TestLocalNetworkIsLeftAlone(t *testing.T) {
	for _, l := range []*link{
		{bps: 1e9, roundTrip: time.Millisecond, drawPerMB: 25 * time.Millisecond},
		{bps: 300e6, roundTrip: 4 * time.Millisecond, drawPerMB: 40 * time.Millisecond}, // wireless, slower client
	} {
		m, _ := run(l, 60)
		if m.Limited() || m.Quality(80) != 80 || m.VideoCap(time.Now()) != 0 || m.Window(16*time.Millisecond) != 2 {
			rate, base := m.Info()
			t.Errorf("a %.0f Mbit/s local link was held back: rate %.1f Mbit/s, round trip %v, quality %d", l.bps/1e6, rate/1e6, base, m.Quality(80))
		}
	}
}

func TestSlowLinkIsMeasured(t *testing.T) {
	for _, mbit := range []float64{5, 15, 30} {
		l := &link{bps: mbit * 1e6, roundTrip: 80 * time.Millisecond, drawPerMB: 25 * time.Millisecond}
		m, worst := run(l, 90)
		rate, base := m.Info()
		if rate < 0.6*l.bps || rate > 1.6*l.bps {
			t.Errorf("%.0f Mbit/s link measured as %.1f Mbit/s", mbit, rate/1e6)
		}
		// On the slowest link even a small frame takes a while, which
		// the round trip measured cannot be told apart from.
		if base < 70*time.Millisecond || base > 150*time.Millisecond {
			t.Errorf("%.0f Mbit/s link: round trip measured as %v", mbit, base)
		}
		if q := m.Quality(80); q >= 80 || q < 30 {
			t.Errorf("%.0f Mbit/s link: quality %d", mbit, q)
		}
		if c := float64(m.VideoCap(time.Now())); c <= 0 || c > 0.9*l.bps {
			t.Errorf("%.0f Mbit/s link: video ceiling %.1f Mbit/s", mbit, c/1e6)
		}
		// Unpaced, a screenful takes 2 MB * 8 / speed: over a second at
		// 15 Mbit/s. Paced, each piece should be acknowledged within a
		// few tenths of a second, so the picture keeps moving.
		unpaced := time.Duration(float64(2<<20)*8/l.bps*float64(time.Second)) + l.roundTrip
		if worst > l.roundTrip+450*time.Millisecond {
			t.Errorf("%.0f Mbit/s link: a frame still took %v (unpaced: %v)", mbit, worst, unpaced)
		}
		t.Logf("%.0f Mbit/s: measured %.1f, round trip %v, quality 80 -> %d, video ceiling %.1f Mbit/s, slowest late frame %v (unpaced %v)",
			mbit, rate/1e6, base.Round(time.Millisecond), m.Quality(80), float64(m.VideoCap(time.Now()))/1e6, worst.Round(time.Millisecond), unpaced.Round(time.Millisecond))
	}
}

// A long round trip alone, on a quick link, must not be mistaken for a slow
// link, and should allow more frames on their way at once.
func TestDistantButQuick(t *testing.T) {
	l := &link{bps: 500e6, roundTrip: 90 * time.Millisecond, drawPerMB: 20 * time.Millisecond}
	m, _ := run(l, 60)
	if m.Limited() {
		rate, _ := m.Info()
		t.Errorf("a quick, distant link was held back to %.1f Mbit/s", rate/1e6)
	}
	if w := m.Window(16 * time.Millisecond); w != 5 {
		t.Errorf("window on a 90 ms round trip at 60 frames a second: %d", w)
	}
}

// When the link gets quicker, the limit lifts.
func TestRecovers(t *testing.T) {
	l := &link{bps: 8e6, roundTrip: 60 * time.Millisecond, drawPerMB: 20 * time.Millisecond}
	var m Meter
	now := time.Unix(1000, 0)
	send := func(bytes int64) {
		if b := m.Budget(); b > 0 && bytes > b {
			bytes = b
		}
		m.Sent(now, bytes)
		ack := l.deliver(now, bytes)
		m.Acked(ack)
		now = ack.Add(10 * time.Millisecond)
	}
	for range 50 {
		send(20 << 10)
		send(400 << 10)
	}
	if !m.Limited() || m.Budget() == 0 {
		t.Fatal("an 8 Mbit/s link was not noticed")
	}
	l.bps = 1e9
	for range 400 {
		send(20 << 10)
		send(400 << 10)
	}
	if m.Limited() {
		rate, _ := m.Info()
		t.Errorf("still held back to %.1f Mbit/s on a link that became quick", rate/1e6)
	}
}

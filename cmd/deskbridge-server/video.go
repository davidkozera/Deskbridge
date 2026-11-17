//go:build windows

package main

// Motion as video.
//
// A still or mostly still screen is sent the usual way: only the areas that
// changed, with text pixel-perfect. When large parts of the screen keep
// changing for a while (a film, scrolling, dragging a window) that becomes
// heavy, so the server switches to a real video codec, using the PC's
// hardware encoder when it has one. Once the motion stops it switches back
// and redraws, crisply, just the areas the video touched.
//
// Short bursts, such as a window opening or closing, never switch: they are
// over before video would pay off, and switching costs more than it saves.
//
// The encoder is started ahead of time, in the background, so that the
// switch itself is instant. It and the client's decoder are kept between
// bursts of motion: every frame reaches the client in order, so the two stay
// in step and a new burst simply continues the same video stream.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"deskbridge/internal/mf"
	"deskbridge/internal/proto"
)

const (
	useTiles = iota
	useVideo
	leaveVideo

	enterFraction = 0.20                   // this much of the screen changing...
	enterAfter    = 500 * time.Millisecond // ...for this long without a pause starts video
	exitFraction  = 0.03                   // less than this changing...
	exitAfter     = 300 * time.Millisecond // ...for this long ends it
	tile          = 64
)

// Encoders that failed are skipped from then on. Index: codec.
var (
	encoderSkip  [3]atomic.Int32
	encoderTotal [3]int
	encoderOnce  [3]sync.Once
)

func encodersLeft(codec byte) bool {
	encoderOnce[codec].Do(func() {
		encoderTotal[codec] = mf.EncoderCount(codec)
		log.Printf("video: %d %s encoder(s) available", encoderTotal[codec], mf.CodecName(codec))
	})
	return int(encoderSkip[codec].Load()) < encoderTotal[codec]
}

// encoderKey is everything an encoder is built for. A change in any of it
// needs a new encoder.
type encoderKey struct {
	codec              byte
	w, h, fps, bitrate int
}

type warmResult struct {
	key encoderKey
	enc *mf.Encoder
	err error
}

type motionVideo struct {
	s                      *session
	active                 bool
	heavySince, quietSince time.Time // zero while not in such a run

	enc       *mf.Encoder
	key       encoderKey
	started   time.Time
	newStream bool
	warming   bool
	warm      chan warmResult

	touched map[[2]int]struct{} // screen tiles drawn by video since the last crisp redraw

	wroteAt    time.Time    // when the last frame sent began to be written, and
	wroteBytes int64        // its size: for measuring the link (see internal/pace)
	areas      []proto.Rect // changed areas not yet sent along with a frame
}

func newMotionVideo(s *session) *motionVideo {
	return &motionVideo{s: s, warm: make(chan warmResult, 1), touched: map[[2]int]struct{}{}}
}

// want says which encoder the current settings call for, if any: H.265 if
// the client can decode it and this PC can encode it, otherwise H.264.
func (m *motionVideo) want(w, h int, interval time.Duration, quality int) (encoderKey, bool) {
	if !m.s.videoOn.Load() || w < 64 || h < 64 {
		return encoderKey{}, false
	}
	mask := byte(m.s.codecs.Load())
	for _, c := range []byte{proto.CodecHEVC, proto.CodecH264} {
		if mask&c == 0 || !encodersLeft(c) {
			continue
		}
		k := encoderKey{codec: c, w: w &^ 1, h: h &^ 1, fps: int(time.Second / max(interval, time.Millisecond))}
		// Bits per pixel from the client's quality setting: about 0.04 at
		// 50, 0.15 at 85.
		bpp := min(max(0.04+float64(quality-50)/45*0.14, 0.03), 0.20)
		// Above 60 frames a second each frame differs less from the last, so
		// the bit rate grows more slowly than the frame rate.
		rate := float64(k.fps)
		if rate > 60 {
			rate = 60 * math.Sqrt(rate/60)
		}
		k.bitrate = int(min(max(float64(k.w*k.h)*rate*bpp, 2e6), 80e6))
		// A link slower than that gets what it can carry.
		if ceiling := m.s.link.VideoCap(time.Now()); ceiling > 0 {
			k.bitrate = min(k.bitrate, ceiling)
		}
		return k, true
	}
	return encoderKey{}, false
}

func (m *motionVideo) dropEncoder() {
	if m.enc != nil {
		m.enc.Close()
		m.enc = nil
	}
}

func (m *motionVideo) close() {
	m.dropEncoder()
	if m.warming { // let the one being started finish, then discard it
		go func() {
			if r := <-m.warm; r.enc != nil {
				r.enc.Close()
			}
		}()
	}
}

// ready makes sure the wanted encoder exists, starting it in the background
// if not, and reports whether it can be used right now. It never waits.
func (m *motionVideo) ready(key encoderKey) bool {
	select {
	case r := <-m.warm:
		m.warming = false
		switch {
		case r.err != nil:
			log.Printf("video: %s encoder unusable, trying the next one: %v", mf.CodecName(r.key.codec), r.err)
			encoderSkip[r.key.codec].Add(1)
		case r.key != key || m.enc != nil:
			r.enc.Close() // settings changed while it was starting
		default:
			m.enc, m.key, m.started, m.newStream = r.enc, r.key, time.Now(), true
			log.Printf("video: ready: %v, %d fps, %.1f Mbit/s", r.enc, key.fps, float64(key.bitrate)/1e6)
			m.s.c.Send(proto.MsgNotice, false, append([]byte{proto.NoticeVideo}, fmt.Sprintf("Motion is being sent as %s video, encoded by: %v.", mf.CodecName(key.codec), r.enc)...))
		}
	default:
	}
	if m.enc != nil && m.key != key {
		m.dropEncoder()
	}
	if m.enc == nil && !m.warming {
		m.warming = true
		skip := int(encoderSkip[key.codec].Load())
		go func() {
			enc, err := mf.NewEncoder(key.codec, key.w, key.h, key.fps, key.bitrate, skip)
			m.warm <- warmResult{key, enc, err}
		}()
	}
	return m.enc != nil
}

// decide looks at how much of the screen changed (area, in pixels) and says
// how this frame should be sent. fresh means a full redraw is due anyway.
//
// On a slow link video starts sooner and for smaller changes: a moving
// picture costs far less as video than as freshly drawn tiles, and there
// the difference is between following the motion and falling behind it.
func (m *motionVideo) decide(area, w, h int, fresh bool, interval time.Duration, quality int) int {
	if m.s.restart.Swap(false) {
		m.dropEncoder() // the next video frame starts a new stream the client can pick up
	}
	key, wanted := m.want(w, h, interval, quality)
	ready := false
	if wanted {
		ready = m.ready(key)
	} else {
		m.dropEncoder() // switched off: the client may drop its decoder, so start afresh next time
	}
	now := time.Now()
	if fresh {
		// Everything is about to be redrawn the ordinary way.
		m.active, m.heavySince, m.quietSince = false, time.Time{}, time.Time{}
		clear(m.touched)
		m.areas = nil
		return useTiles
	}
	frac := float64(area) / float64(max(w*h, 1))
	enterAt, enterWait := enterFraction, enterAfter
	if m.s.link.Limited() {
		enterAt, enterWait = 0.06, 100*time.Millisecond
	}

	if !m.active {
		if frac < enterAt {
			m.heavySince = time.Time{}
			return useTiles
		}
		if m.heavySince.IsZero() {
			m.heavySince = now
		}
		if !ready || now.Sub(m.heavySince) < enterWait {
			return useTiles
		}
		m.active, m.heavySince, m.quietSince = true, time.Time{}, time.Time{}
		return useVideo
	}
	if !ready {
		m.active = false
		return leaveVideo
	}
	if frac >= exitFraction {
		m.quietSince = time.Time{}
	} else if m.quietSince.IsZero() {
		m.quietSince = now
	}
	if !m.quietSince.IsZero() && now.Sub(m.quietSince) >= exitAfter {
		m.active = false
		return leaveVideo
	}
	return useVideo
}

// send encodes the captured frame and sends what the encoder produced,
// together with the areas that changed so the client redraws only those.
// It reports whether anything was sent.
func (m *motionVideo) send(pix []byte, pixW int, rects []proto.Rect) (bool, error) {
	if m.enc == nil {
		m.active = false
		return false, errors.New("no encoder")
	}
	k := m.key
	for _, r := range rects {
		for y := r.Y; y < r.Y+r.H; y += tile {
			for x := r.X; x < r.X+r.W; x += tile {
				m.touched[[2]int{x / tile, y / tile}] = struct{}{}
			}
		}
	}
	if m.areas = append(m.areas, rects...); len(m.areas) > 2000 {
		m.areas = []proto.Rect{{X: 0, Y: 0, W: k.w, H: k.h}}
	}

	pkts, err := m.enc.Encode(func(dst []byte) { proto.BGRAToNV12(dst, pix, pixW, k.w, k.h) }, time.Since(m.started))
	if err != nil {
		log.Printf("video: %s stopped working, trying the next encoder: %v", m.enc.Name, err)
		encoderSkip[k.codec].Add(1)
		m.dropEncoder()
		m.active = false
		return false, err
	}
	m.wroteAt, m.wroteBytes = time.Now(), 0
	for _, p := range pkts {
		hdr := make([]byte, 8, 8+8*len(m.areas))
		if p.Key {
			hdr[0] |= 1
		}
		if m.newStream {
			hdr[0] |= 2
			m.newStream = false
		}
		hdr[1] = k.codec
		binary.LittleEndian.PutUint16(hdr[2:], uint16(k.w))
		binary.LittleEndian.PutUint16(hdr[4:], uint16(k.h))
		binary.LittleEndian.PutUint16(hdr[6:], uint16(len(m.areas)))
		for _, r := range m.areas {
			for _, v := range []int{r.X, r.Y, r.W, r.H} {
				hdr = binary.LittleEndian.AppendUint16(hdr, uint16(v))
			}
		}
		m.areas = m.areas[:0] // sent with the first frame; later ones add nothing
		m.wroteBytes += int64(len(hdr) + len(p.Data) + 5)
		if err := m.s.c.Send(proto.MsgVideo, false, hdr, p.Data); err != nil {
			return false, err
		}
	}
	return len(pkts) > 0, nil
}

// markForRedraw makes the ordinary method resend, pixel-perfect, every tile
// that video drew: it spoils one byte of each such tile in the remembered
// frame so the next comparison finds it changed.
func (m *motionVideo) markForRedraw(prev []byte, w int) {
	for t := range m.touched {
		if o := (t[1]*tile*w + t[0]*tile) * 4; o < len(prev) {
			prev[o] ^= 0xFF
		}
	}
	clear(m.touched)
	m.areas = nil
}

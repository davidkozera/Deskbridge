//go:build windows

package mf

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

// Packet is one encoded video frame.
type Packet struct {
	Data []byte
	Key  bool
}

// Encoder compresses NV12 pictures to H.265 or H.264.
type Encoder struct {
	Name string // which encoder Windows gave us, for the log

	w              *worker
	activate, mft  uintptr
	events         uintptr
	async          bool
	inID, outID    uint32
	width, height  int
	frame100ns     int64
	header         []byte // codec parameter sets, sent ahead of the first frame if the encoder leaves them out
	first          bool
	need           chan struct{}
	out            chan Packet
	wait           time.Duration
	misses, silent int
	closed         atomic.Bool
	loopDone       chan struct{}
}

type variant struct {
	vt  uint16
	_   [3]uint16
	val uint64
	_   uint64
}

func setCodecU32(api uintptr, key *guid, v uint32) uint32 {
	x := variant{vt: 19, val: uint64(v)} // VT_UI4
	return call(api, 9, gp(key), uintptr(unsafe.Pointer(&x)))
}

func setCodecBool(api uintptr, key *guid, on bool) uint32 {
	x := variant{vt: 11} // VT_BOOL
	if on {
		x.val = 0xFFFF
	}
	return call(api, 9, gp(key), uintptr(unsafe.Pointer(&x)))
}

// EncoderCount reports how many encoders Windows offers for the codec.
// NewEncoder's skip argument selects among them.
func EncoderCount(codec byte) int {
	n := 0
	w := newWorker()
	w.do(func() {
		acts := encoderCandidates(codec)
		n = len(acts)
		for _, a := range acts {
			release(a)
		}
	})
	w.stop()
	return n
}

// encoderCandidates lists encoders for the codec: hardware first, then
// software.
func encoderCandidates(codec byte) []uintptr {
	out := registerTypeInfo{mediaTypeVideo, *codecFormat(codec)}
	acts := enumerate(&categoryVideoEncoder, enumHardware|enumSortAndFilter, nil, &out)
	return append(acts, enumerate(&categoryVideoEncoder, enumSync|enumAsync|enumLocal|enumSortAndFilter, nil, &out)...)
}

// NewEncoder sets up the skip-th available encoder for width×height (both
// even) at the given frame rate and bit rate.
func NewEncoder(codec byte, width, height, fps, bitrate, skip int) (*Encoder, error) {
	e := &Encoder{w: newWorker(), width: width, height: height, frame100ns: int64(10000000 / max(fps, 1)),
		first: true, need: make(chan struct{}, 256), out: make(chan Packet, 256), wait: 60 * time.Millisecond}
	var err error
	e.w.do(func() { err = e.setup(codec, fps, bitrate, skip) })
	if err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

func (e *Encoder) setup(codec byte, fps, bitrate, skip int) error {
	acts := encoderCandidates(codec)
	if skip >= len(acts) {
		for _, a := range acts {
			release(a)
		}
		return errNone
	}
	for i, a := range acts {
		if i != skip {
			release(a)
		}
	}
	e.activate = acts[skip]
	e.Name = nameOf(e.activate)
	if e.mft = activateMFT(e.activate); e.mft == 0 {
		return errors.New(e.Name + ": could not be started")
	}
	mft := e.mft

	var attrs uintptr
	if ok(call(mft, mftGetAttributes, uintptr(unsafe.Pointer(&attrs)))) && attrs != 0 {
		if v, found := getU32(attrs, &transformAsync); found && v != 0 {
			e.async = true
			setU32(attrs, &transformAsyncUnlock, 1)
		}
		setU32(attrs, &lowLatency, 1)
		release(attrs)
	}
	var in, out uint32
	if ok(call(mft, mftGetStreamIDs, 1, uintptr(unsafe.Pointer(&in)), 1, uintptr(unsafe.Pointer(&out)))) {
		e.inID, e.outID = in, out
	}

	// Tuning for a live picture: answer each frame at once, no reordering,
	// and effectively never start over with a full frame. All optional:
	// encoders ignore what they do not support.
	var api uintptr
	if ok(call(mft, 0, gp(&iidCodecAPI), uintptr(unsafe.Pointer(&api)))) && api != 0 {
		setCodecBool(api, &lowLatency, true)
		if !ok(setCodecU32(api, &codecRateControl, 4)) { // low-delay variable bit rate
			setCodecU32(api, &codecRateControl, 0) // constant bit rate
		}
		setCodecU32(api, &codecMeanBitRate, uint32(bitrate))
		setCodecU32(api, &codecBFrames, 0)
		setCodecU32(api, &codecGOPSize, uint32(fps*3600))
		release(api)
	}

	size, rate := pair(e.width, e.height), pair(fps, 1)
	ot := newMediaType()
	defer release(ot)
	setGUID(ot, &mtMajorType, &mediaTypeVideo)
	setGUID(ot, &mtSubtype, codecFormat(codec))
	setU32(ot, &mtAvgBitrate, uint32(bitrate))
	setU64(ot, &mtFrameSize, size)
	setU64(ot, &mtFrameRate, rate)
	setU64(ot, &mtPixelAspect, pair(1, 1))
	setU32(ot, &mtInterlaceMode, progressiveScan)
	if codec == proto.CodecH264 {
		setU32(ot, &mtProfile, 100) // High
	} else {
		setU32(ot, &mtProfile, 1) // Main
	}
	if hr := call(mft, mftSetOutputType, uintptr(e.outID), ot, 0); !ok(hr) {
		return hrErr(e.Name+": output format refused", hr)
	}

	var hr uint32
	for _, full := range []bool{true, false} { // second try with only the essentials
		it := newMediaType()
		setGUID(it, &mtMajorType, &mediaTypeVideo)
		setGUID(it, &mtSubtype, &formatNV12)
		setU64(it, &mtFrameSize, size)
		setU64(it, &mtFrameRate, rate)
		setU64(it, &mtPixelAspect, pair(1, 1))
		setU32(it, &mtInterlaceMode, progressiveScan)
		if full {
			setU32(it, &mtDefaultStride, uint32(e.width))
			setU32(it, &mtNominalRange, nominalRange16to235)
			setU32(it, &mtYUVMatrix, matrixBT709)
		}
		hr = call(mft, mftSetInputType, uintptr(e.inID), it, 0)
		release(it)
		if ok(hr) {
			break
		}
	}
	if !ok(hr) {
		return hrErr(e.Name+": input format refused", hr)
	}

	var cur uintptr
	if ok(call(mft, mftGetOutputCurrentType, uintptr(e.outID), uintptr(unsafe.Pointer(&cur)))) && cur != 0 {
		var n uint32
		if ok(call(cur, attrGetBlobSize, gp(&mtSequenceHeader), uintptr(unsafe.Pointer(&n)))) && n > 0 && n < 4096 {
			b := make([]byte, n)
			if ok(call(cur, attrGetBlob, gp(&mtSequenceHeader), uintptr(unsafe.Pointer(&b[0])), uintptr(n), 0)) {
				e.header = b
			}
		}
		release(cur)
	}

	if e.async {
		if !ok(call(mft, 0, gp(&iidEventGenerator), uintptr(unsafe.Pointer(&e.events)))) || e.events == 0 {
			return errors.New(e.Name + ": no event source")
		}
		e.loopDone = make(chan struct{})
		go e.eventLoop(e.events)
	}
	call(mft, mftProcessMessage, msgBeginStreaming, 0)
	call(mft, mftProcessMessage, msgStartOfStream, 0)
	return nil
}

// eventLoop relays a hardware encoder's "ready for a frame" and "a frame is
// done" notices. It ends when the encoder is shut down.
func (e *Encoder) eventLoop(events uintptr) {
	runtime.LockOSThread()
	initThread()
	defer close(e.loopDone)
	for !e.closed.Load() {
		var ev uintptr
		if !ok(call(events, 3, 0, uintptr(unsafe.Pointer(&ev)))) || ev == 0 { // GetEvent, waits
			return
		}
		var kind uint32
		call(ev, 33, uintptr(unsafe.Pointer(&kind))) // GetType
		release(ev)
		switch kind {
		case eventNeedInput:
			select {
			case e.need <- struct{}{}:
			default:
			}
		case eventHaveOutput:
			if p, got := e.take(); got {
				select {
				case e.out <- p:
				default:
				}
			}
		}
	}
}

// take pulls one finished frame out of the encoder.
func (e *Encoder) take() (Packet, bool) {
	for try := 0; try < 2; try++ {
		sample, result, _ := pull(e.mft, e.outID)
		switch result {
		case pullGot:
			var p Packet
			withSampleData(sample, func(data []byte) { p.Data = append([]byte(nil), data...) })
			if v, found := getU32(sample, &sampleCleanPoint); found && v != 0 {
				p.Key = true
			}
			release(sample)
			return p, len(p.Data) > 0
		case pullStreamChange:
			// The encoder wants its output format confirmed again.
			var t uintptr
			if ok(call(e.mft, mftGetOutputAvailableType, uintptr(e.outID), 0, uintptr(unsafe.Pointer(&t)))) && t != 0 {
				call(e.mft, mftSetOutputType, uintptr(e.outID), t, 0)
				release(t)
			}
		default:
			return Packet{}, false
		}
	}
	return Packet{}, false
}

var ErrEncoderStuck = errors.New("the encoder is not producing video")

// Encode compresses one picture. fill must write NV12Size(width, height)
// bytes. It returns the frames that came out, usually exactly one; none is
// normal now and then, but a long run of none is reported as an error.
func (e *Encoder) Encode(fill func(dst []byte), elapsed time.Duration) (pkts []Packet, err error) {
	e.w.do(func() { pkts, err = e.encode(fill, elapsed) })
	return
}

func (e *Encoder) encode(fill func(dst []byte), elapsed time.Duration) ([]Packet, error) {
	if e.async {
		select {
		case <-e.need:
		case <-time.After(time.Second):
			if e.silent++; e.silent > 3 {
				return nil, ErrEncoderStuck
			}
			return nil, nil
		}
	}
	sample := newSample(proto.NV12Size(e.width, e.height), fill, int64(elapsed/100), e.frame100ns)
	if sample == 0 {
		return nil, errors.New("out of memory")
	}
	hr := call(e.mft, mftProcessInput, uintptr(e.inID), sample, 0)
	release(sample)
	if !ok(hr) {
		return nil, hrErr(e.Name+": frame refused", hr)
	}
	var pkts []Packet
	if e.async {
		select {
		case p := <-e.out:
			pkts = append(pkts, p)
			e.misses = 0
		case <-time.After(e.wait):
			// This encoder answers a frame late. Stop waiting on it so the
			// frame rate holds; its frames arrive one call behind.
			if e.misses++; e.misses >= 2 {
				e.wait = 8 * time.Millisecond
			}
		}
	drain:
		for {
			select {
			case p := <-e.out:
				pkts = append(pkts, p)
			default:
				break drain
			}
		}
	} else {
		for {
			p, got := e.take()
			if !got {
				break
			}
			pkts = append(pkts, p)
		}
	}
	if len(pkts) == 0 {
		if e.silent++; e.silent > 45 {
			return nil, ErrEncoderStuck
		}
		return nil, nil
	}
	e.silent = 0
	if e.first {
		e.first = false
		if len(e.header) > 0 && !bytes.Contains(pkts[0].Data[:min(len(pkts[0].Data), len(e.header)+256)], e.header) {
			pkts[0].Data = append(append([]byte(nil), e.header...), pkts[0].Data...)
		}
	}
	return pkts, nil
}

// Close shuts the encoder down and frees it.
func (e *Encoder) Close() {
	if e.closed.Swap(true) {
		return
	}
	e.w.do(func() {
		if e.mft != 0 {
			call(e.mft, mftProcessMessage, msgEndStreaming, 0)
			call(e.mft, mftProcessMessage, msgFlush, 0)
		}
		if e.activate != 0 {
			call(e.activate, 34) // ShutdownObject: also wakes the event loop
		}
		if e.loopDone != nil {
			select { // let the event loop finish before the encoder is freed under it
			case <-e.loopDone:
			case <-time.After(time.Second):
			}
		}
		release(e.events)
		release(e.mft)
		release(e.activate)
		e.events, e.mft, e.activate = 0, 0, 0
	})
	e.w.stop()
}

func (e *Encoder) String() string {
	kind := "software or synchronous"
	if e.async {
		kind = "hardware"
	}
	return fmt.Sprintf("%s (%s) %dx%d", e.Name, kind, e.width, e.height)
}

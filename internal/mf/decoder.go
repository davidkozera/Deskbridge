//go:build windows

package mf

import (
	"errors"
	"syscall"
	"unsafe"

	"deskbridge/internal/proto"
)

// Decoding on the graphics card.
//
// Windows' decoders use the graphics card's video decoder (DXVA) when they
// are handed a Direct3D 11 device through a "device manager". The decoded
// picture then lives in graphics memory; locking its buffer copies it back
// so it can be drawn the same way as before.

var (
	d3d11 = syscall.NewLazyDLL("d3d11.dll")

	pD3D11CreateDevice         = d3d11.NewProc("D3D11CreateDevice")
	pMFCreateDXGIDeviceManager = mfplat.NewProc("MFCreateDXGIDeviceManager")

	iidMultithread = guid{0x9b7e4e00, 0x342c, 0x4106, [8]byte{0xa1, 0x9f, 0x4f, 0x27, 0x04, 0xf6, 0x89, 0xf0}}
	iidDXGIBuffer  = guid{0xe7174cfa, 0x1c9e, 0x48b1, [8]byte{0x88, 0x66, 0x62, 0x62, 0x26, 0xbf, 0xc2, 0x58}}
)

const msgSetD3DManager = 2

// newDeviceManager creates a Direct3D 11 device able to decode video and
// wraps it in the manager decoders expect.
func newDeviceManager() (manager, device uintptr) {
	if pD3D11CreateDevice.Find() != nil || pMFCreateDXGIDeviceManager.Find() != nil {
		return 0, 0
	}
	var ctx uintptr
	var level uint32
	for _, flags := range []uintptr{0x800 | 0x20, 0x800} { // video support (+ BGRA)
		hr, _, _ := pD3D11CreateDevice.Call(0, 1 /* hardware */, 0, flags, 0, 0, 7,
			uintptr(unsafe.Pointer(&device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&ctx)))
		if ok(uint32(hr)) && device != 0 {
			break
		}
		device = 0
	}
	if device == 0 {
		return 0, 0
	}
	release(ctx)
	// The decoder uses the device from its own threads.
	var mt uintptr
	if ok(call(device, 0, gp(&iidMultithread), uintptr(unsafe.Pointer(&mt)))) && mt != 0 {
		call(mt, 5, 1) // SetMultithreadProtected(TRUE)
		release(mt)
	}
	var token uint32
	if hr, _, _ := pMFCreateDXGIDeviceManager.Call(uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&manager))); !ok(uint32(hr)) || manager == 0 {
		release(device)
		return 0, 0
	}
	if !ok(call(manager, 7, device, uintptr(token))) { // ResetDevice
		release(manager)
		release(device)
		return 0, 0
	}
	return manager, device
}

// Decoder turns H.265 or H.264 frames back into NV12 pictures.
type Decoder struct {
	Name string

	w             *worker
	mft           uintptr
	width, height int // the picture we expect
	codedW        int // what the decoder actually hands back: rows may be
	codedH        int // padded and there may be extra rows at the bottom
	stride        int
	frame         int64

	manager, device uintptr // set while decoding on the graphics card
	wantGPU         bool
	checked, onGPU  bool
}

// OnGPU reports whether pictures are coming from the graphics card's
// decoder. It is only meaningful once a picture has been decoded.
func (d *Decoder) OnGPU() bool { return d.onGPU }

// AskedGPU reports whether the decoder was set up to use the graphics card.
func (d *Decoder) AskedGPU() bool { return d.manager != 0 }

func decoderCandidates(codec byte) []uintptr {
	in := registerTypeInfo{mediaTypeVideo, *codecFormat(codec)}
	return enumerate(&categoryVideoDecoder, enumSync|enumLocal|enumSortAndFilter, &in, nil)
}

// CanDecode reports whether this PC has a decoder for the codec. H.264 is
// built into Windows; H.265 needs the "HEVC Video Extensions" add-on.
func CanDecode(codec byte) bool {
	n := 0
	w := newWorker()
	w.do(func() {
		acts := decoderCandidates(codec)
		n = len(acts)
		for _, a := range acts {
			release(a)
		}
	})
	w.stop()
	return n > 0
}

// NewDecoder sets up a decoder. With gpu it decodes on the graphics card if
// the decoder and the card allow it, and in software otherwise.
func NewDecoder(codec byte, width, height int, gpu bool) (*Decoder, error) {
	d := &Decoder{w: newWorker(), width: width, height: height, wantGPU: gpu}
	var err error
	d.w.do(func() { err = d.setup(codec) })
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *Decoder) setup(codec byte) error {
	acts := decoderCandidates(codec)
	defer func() {
		for _, a := range acts {
			release(a)
		}
	}()
	err := errNone
	for _, a := range acts {
		mft := activateMFT(a)
		if mft == 0 {
			continue
		}
		var attrs uintptr
		if ok(call(mft, mftGetAttributes, uintptr(unsafe.Pointer(&attrs)))) && attrs != 0 {
			setU32(attrs, &lowLatency, 1) // show each frame as it arrives
			release(attrs)
		}
		if d.wantGPU {
			if d.manager == 0 {
				d.manager, d.device = newDeviceManager()
			}
			if d.manager != 0 && !ok(call(mft, mftProcessMessage, msgSetD3DManager, d.manager)) {
				// This decoder cannot use the graphics card: software it is.
				release(d.manager)
				release(d.device)
				d.manager, d.device = 0, 0
			}
		}
		it := newMediaType()
		setGUID(it, &mtMajorType, &mediaTypeVideo)
		setGUID(it, &mtSubtype, codecFormat(codec))
		setU64(it, &mtFrameSize, pair(d.width, d.height))
		setU64(it, &mtPixelAspect, pair(1, 1))
		setU32(it, &mtInterlaceMode, progressiveScan)
		hr := call(mft, mftSetInputType, 0, it, 0)
		release(it)
		if !ok(hr) {
			err = hrErr(nameOf(a)+": input format refused", hr)
			release(mft)
			continue
		}
		d.mft = mft
		if !d.chooseOutput() {
			err = errors.New(nameOf(a) + ": no NV12 output")
			d.mft = 0
			release(mft)
			continue
		}
		d.Name = nameOf(a)
		call(mft, mftProcessMessage, msgBeginStreaming, 0)
		call(mft, mftProcessMessage, msgStartOfStream, 0)
		return nil
	}
	return err
}

// chooseOutput selects NV12 from the formats the decoder offers and notes
// the layout of the pictures it will produce.
func (d *Decoder) chooseOutput() bool {
	for i := 0; i < 64; i++ {
		var t uintptr
		if !ok(call(d.mft, mftGetOutputAvailableType, 0, uintptr(i), uintptr(unsafe.Pointer(&t)))) || t == 0 {
			return false
		}
		var sub guid
		call(t, attrGetGUID, gp(&mtSubtype), uintptr(unsafe.Pointer(&sub)))
		if sub != formatNV12 {
			release(t)
			continue
		}
		hr := call(d.mft, mftSetOutputType, 0, t, 0)
		d.codedW, d.codedH = d.width, d.height
		if v, found := getU64(t, &mtFrameSize); found {
			d.codedW, d.codedH = int(v>>32), int(uint32(v))
		}
		d.stride = d.codedW
		if v, found := getU32(t, &mtDefaultStride); found && int32(v) > 0 {
			d.stride = int(v)
		}
		release(t)
		return ok(hr)
	}
	return false
}

// Decode feeds one encoded frame. show is called for each picture that comes
// out (usually one), with the brightness and colour planes and their row
// length; the slices are only valid during the call.
func (d *Decoder) Decode(frame []byte, show func(y, uv []byte, stride int)) (err error) {
	d.w.do(func() { err = d.decode(frame, show) })
	return
}

func (d *Decoder) decode(frame []byte, show func(y, uv []byte, stride int)) error {
	d.frame++
	sample := newSample(len(frame), func(dst []byte) { copy(dst, frame) }, d.frame*333333, 333333)
	if sample == 0 {
		return errors.New("out of memory")
	}
	defer release(sample)
	hr := call(d.mft, mftProcessInput, 0, sample, 0)
	if hr == hrNotAccepting { // it wants its finished pictures collected first
		if err := d.drain(show); err != nil {
			return err
		}
		hr = call(d.mft, mftProcessInput, 0, sample, 0)
	}
	if !ok(hr) {
		return hrErr(d.Name+": frame refused", hr)
	}
	return d.drain(show)
}

func (d *Decoder) drain(show func(y, uv []byte, stride int)) error {
	for changes := 0; ; {
		sample, result, hr := pull(d.mft, 0)
		switch result {
		case pullNeedInput:
			return nil
		case pullStreamChange:
			// Normal once at the start: the decoder has read the real
			// picture size from the stream.
			if changes++; changes > 4 || !d.chooseOutput() {
				return errors.New(d.Name + ": could not settle on an output format")
			}
		case pullGot:
			if !d.checked {
				// A picture held in graphics memory means the card decoded it.
				d.checked = true
				var buf, dxgi uintptr
				if ok(call(sample, 40, 0, uintptr(unsafe.Pointer(&buf)))) && buf != 0 { // GetBufferByIndex
					if ok(call(buf, 0, gp(&iidDXGIBuffer), uintptr(unsafe.Pointer(&dxgi)))) && dxgi != 0 {
						d.onGPU = true
						release(dxgi)
					}
					release(buf)
				}
			}
			laidOut := false
			withSampleData(sample, func(data []byte) {
				stride, rows, found := proto.PlaneLayout(len(data), d.width, d.height, d.stride, d.codedH)
				if !found {
					return
				}
				laidOut = true
				show(data[:stride*rows], data[stride*rows:], stride)
			})
			release(sample)
			if !laidOut {
				return errors.New(d.Name + ": unexpected picture layout")
			}
		default:
			return hrErr(d.Name+": decoding failed", hr)
		}
	}
}

func (d *Decoder) Close() {
	if d.w == nil {
		return
	}
	d.w.do(func() {
		if d.mft != 0 {
			call(d.mft, mftProcessMessage, msgEndStreaming, 0)
			release(d.mft)
			d.mft = 0
		}
		release(d.manager)
		release(d.device)
		d.manager, d.device = 0, 0
	})
	d.w.stop()
	d.w = nil
}

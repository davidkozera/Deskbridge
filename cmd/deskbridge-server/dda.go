//go:build windows

package main

// Fast screen capture with the Desktop Duplication API.
//
// The original capture asks Windows to copy the whole screen into memory
// thirty times a second, changed or not. Desktop Duplication instead gets
// each finished frame from the graphics card, and only when something on
// screen actually changed: quicker per frame, and nearly free when idle.
//
// A duplication is tied to the desktop it was created on (the normal one,
// or the secure one that shows the lock screen and admin prompts) and to the
// thread that created it. So each one lives on a thread of its own, which
// attaches itself to whichever desktop is showing; when the desktop or the
// display mode changes, Windows reports the duplication lost and the caller
// simply makes a new one.

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	dxgi  = syscall.NewLazyDLL("dxgi.dll")
	d3d11 = syscall.NewLazyDLL("d3d11.dll")

	pCreateDXGIFactory1 = dxgi.NewProc("CreateDXGIFactory1")
	pD3D11CreateDevice  = d3d11.NewProc("D3D11CreateDevice")

	iidDXGIFactory1   = guid{0x770aae78, 0xf26f, 0x4dba, [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidDXGIOutput1    = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidD3D11Texture2D = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

const (
	dxgiWaitTimeout = 0x887A0027

	// Method numbers in the COM interfaces used.
	factoryEnumAdapters1   = 12
	adapterEnumOutputs     = 7
	outputGetDesc          = 7
	output1DuplicateOutput = 22
	duplAcquireNextFrame   = 8
	duplReleaseFrame       = 14
	deviceCreateTexture2D  = 5
	textureGetDesc         = 10
	contextMap             = 14
	contextUnmap           = 15
	contextCopyResource    = 47

	formatBGRA8     = 87
	formatBGRA8SRGB = 91
)

// Results of asking for a frame.
const (
	ddaFrame = iota // the buffer now holds a new picture
	ddaSame         // nothing on screen changed
	ddaLost         // the duplication is no longer valid; make a new one
)

// hcall invokes method number idx on a COM object and returns its HRESULT.
func hcall(obj uintptr, idx int, args ...uintptr) uint32 {
	vtbl := *(*unsafe.Pointer)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return uint32(hr)
}

func hok(hr uint32) bool { return int32(hr) >= 0 }

type outputDesc struct {
	name      [32]uint16
	left, top int32
	right     int32
	bottom    int32
	attached  int32
	rotation  uint32
	monitor   uintptr
}

type textureDesc struct {
	width, height uint32
	mipLevels     uint32
	arraySize     uint32
	format        uint32
	sampleCount   uint32
	sampleQuality uint32
	usage         uint32
	bindFlags     uint32
	cpuAccess     uint32
	misc          uint32
}

type frameInfo struct {
	lastPresentTime   int64
	lastMouseUpdate   int64
	accumulatedFrames uint32
	rectsCoalesced    int32
	protectedMasked   int32
	pointerX          int32
	pointerY          int32
	pointerVisible    int32
	metadataSize      uint32
	pointerShapeSize  uint32
}

type mappedSubresource struct {
	data       unsafe.Pointer
	rowPitch   uint32
	depthPitch uint32
}

// dda is one desktop duplication and the thread it lives on.
type dda struct {
	jobs chan func()

	// Used only on the duplication's own thread.
	desktop            uintptr
	device, context    uintptr
	dupl, staging      uintptr
	stagingW, stagingH uint32
	primed             bool
}

// newDDA starts duplicating the primary monitor of whichever desktop is
// showing right now.
func newDDA() (*dda, error) {
	d := &dda{jobs: make(chan func())}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread ends with this goroutine
		ready <- d.init()
		for f := range d.jobs {
			f()
		}
		d.release()
	}()
	if err := <-ready; err != nil {
		close(d.jobs)
		return nil, err
	}
	return d, nil
}

func (d *dda) close() { close(d.jobs) }

func (d *dda) init() error {
	if pCreateDXGIFactory1.Find() != nil || pD3D11CreateDevice.Find() != nil {
		return errors.New("Direct3D 11 is not available")
	}
	// A new thread starts on the normal desktop; move to the one showing.
	if h, _, _ := pOpenInputDesktop.Call(0, 0, 0x10000000); h != 0 {
		if r, _, _ := pSetThreadDesktop.Call(h); r != 0 {
			d.desktop = h
		} else {
			pCloseDesktop.Call(h)
		}
	}

	var factory uintptr
	if hr, _, _ := pCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); !hok(uint32(hr)) || factory == 0 {
		return fmt.Errorf("no DXGI factory: 0x%08X", uint32(hr))
	}
	defer release(factory)

	// Find the primary monitor (the one whose top-left corner is 0,0) and
	// the graphics adapter driving it.
	var adapter, output uintptr
	for a := uintptr(0); a < 16 && output == 0; a++ {
		var ad uintptr
		if !hok(hcall(factory, factoryEnumAdapters1, a, uintptr(unsafe.Pointer(&ad)))) || ad == 0 {
			break
		}
		for o := uintptr(0); o < 16; o++ {
			var out uintptr
			if !hok(hcall(ad, adapterEnumOutputs, o, uintptr(unsafe.Pointer(&out)))) || out == 0 {
				break
			}
			var desc outputDesc
			if hok(hcall(out, outputGetDesc, uintptr(unsafe.Pointer(&desc)))) && desc.attached != 0 && desc.left == 0 && desc.top == 0 {
				if desc.rotation > 1 { // 0 = unspecified, 1 = not rotated
					release(out)
					release(ad)
					return errors.New("the display is rotated")
				}
				adapter, output = ad, out
				break
			}
			release(out)
		}
		if output == 0 {
			release(ad)
		}
	}
	if output == 0 {
		return errors.New("primary monitor not found")
	}
	defer release(adapter)
	defer release(output)

	var level uint32
	// Driver type 0 ("unknown") is required when naming an adapter.
	hr, _, _ := pD3D11CreateDevice.Call(adapter, 0, 0, 0x20 /* BGRA support */, 0, 0, 7,
		uintptr(unsafe.Pointer(&d.device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&d.context)))
	if !hok(uint32(hr)) || d.device == 0 || d.context == 0 {
		return fmt.Errorf("no Direct3D device: 0x%08X", uint32(hr))
	}
	var output1 uintptr
	if !hok(hcall(output, 0, uintptr(unsafe.Pointer(&iidDXGIOutput1)), uintptr(unsafe.Pointer(&output1)))) || output1 == 0 {
		return errors.New("the display does not support duplication")
	}
	defer release(output1)
	if hr := hcall(output1, output1DuplicateOutput, d.device, uintptr(unsafe.Pointer(&d.dupl))); !hok(hr) || d.dupl == 0 {
		return fmt.Errorf("duplication refused: 0x%08X", hr)
	}
	return nil
}

func (d *dda) release() {
	release(d.staging)
	release(d.dupl)
	release(d.context)
	release(d.device)
	d.staging, d.dupl, d.context, d.device = 0, 0, 0, 0
	if d.desktop != 0 {
		// The thread is about to end; its desktop handle can go.
		pCloseDesktop.Call(d.desktop)
		d.desktop = 0
	}
}

// grab waits up to waitMs for the screen to change and, if it did, copies
// the new picture into pix (w×h, BGRA, top row first).
func (d *dda) grab(pix []byte, w, h, waitMs int) (result int) {
	done := make(chan struct{})
	d.jobs <- func() { result = d.grabOnThread(pix, w, h, waitMs); close(done) }
	<-done
	return
}

func (d *dda) grabOnThread(pix []byte, w, h, waitMs int) int {
	var info frameInfo
	var resource uintptr
	hr := hcall(d.dupl, duplAcquireNextFrame, uintptr(waitMs), uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&resource)))
	if hr == dxgiWaitTimeout {
		return ddaSame
	}
	if !hok(hr) || resource == 0 {
		return ddaLost // desktop switched, display mode changed, or similar
	}
	defer hcall(d.dupl, duplReleaseFrame)
	defer release(resource)
	if info.lastPresentTime == 0 && d.primed {
		return ddaSame // only the mouse pointer moved
	}

	var tex uintptr
	if !hok(hcall(resource, 0, uintptr(unsafe.Pointer(&iidD3D11Texture2D)), uintptr(unsafe.Pointer(&tex)))) || tex == 0 {
		return ddaLost
	}
	defer release(tex)
	var desc textureDesc
	hcall(tex, textureGetDesc, uintptr(unsafe.Pointer(&desc)))
	if int(desc.width) != w || int(desc.height) != h || (desc.format != formatBGRA8 && desc.format != formatBGRA8SRGB) {
		return ddaLost // not the picture we expect; let the caller sort it out
	}

	// The frame is in graphics memory. Copy it to a texture the processor
	// can read, then into pix.
	if d.staging == 0 || d.stagingW != desc.width || d.stagingH != desc.height {
		release(d.staging)
		d.staging = 0
		sd := textureDesc{width: desc.width, height: desc.height, mipLevels: 1, arraySize: 1, format: desc.format,
			sampleCount: 1, usage: 3 /* staging */, cpuAccess: 0x20000 /* read */}
		if !hok(hcall(d.device, deviceCreateTexture2D, uintptr(unsafe.Pointer(&sd)), 0, uintptr(unsafe.Pointer(&d.staging)))) || d.staging == 0 {
			d.staging = 0
			return ddaLost
		}
		d.stagingW, d.stagingH = desc.width, desc.height
	}
	hcall(d.context, contextCopyResource, d.staging, tex)
	var m mappedSubresource
	if !hok(hcall(d.context, contextMap, d.staging, 0, 1 /* read */, 0, uintptr(unsafe.Pointer(&m)))) || m.data == nil {
		return ddaLost
	}
	pitch, row := int(m.rowPitch), w*4
	if pitch < row || len(pix) < row*h {
		hcall(d.context, contextUnmap, d.staging, 0)
		return ddaLost
	}
	src := unsafe.Slice((*byte)(m.data), pitch*(h-1)+row)
	if pitch == row {
		copy(pix, src)
	} else {
		for y := 0; y < h; y++ {
			copy(pix[y*row:(y+1)*row], src[y*pitch:y*pitch+row])
		}
	}
	hcall(d.context, contextUnmap, d.staging, 0)
	d.primed = true
	return ddaFrame
}

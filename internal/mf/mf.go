//go:build windows

// Package mf drives Windows Media Foundation video encoders and decoders
// ("MFTs") directly, without cgo. The server uses an encoder, preferably the
// hardware one built into the PC's chip; the client uses a decoder.
//
// Every encoder and decoder runs on a thread of its own. Callers hand work to
// that thread and wait for the answer, so they never touch COM themselves.
package mf

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"deskbridge/internal/proto"
)

var (
	mfplat = syscall.NewLazyDLL("mfplat.dll")
	ole32  = syscall.NewLazyDLL("ole32.dll")

	pMFStartup            = mfplat.NewProc("MFStartup")
	pMFCreateMediaType    = mfplat.NewProc("MFCreateMediaType")
	pMFCreateSample       = mfplat.NewProc("MFCreateSample")
	pMFCreateMemoryBuffer = mfplat.NewProc("MFCreateMemoryBuffer")
	pMFTEnumEx            = mfplat.NewProc("MFTEnumEx")
	pCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	pCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

type guid struct {
	d1 uint32
	d2 uint16
	d3 uint16
	d4 [8]byte
}

func fourCC(s string) guid {
	return guid{uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24, 0x0000, 0x0010,
		[8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
}

var (
	mediaTypeVideo = fourCC("vids")
	formatNV12     = fourCC("NV12")
	formatHEVC     = fourCC("HEVC")
	formatH264     = fourCC("H264")

	categoryVideoEncoder = guid{0xf79eac7d, 0xe545, 0x4387, [8]byte{0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a}}
	categoryVideoDecoder = guid{0xd6c02d4b, 0x6833, 0x45b4, [8]byte{0x97, 0x1a, 0x05, 0xa4, 0xb0, 0x4b, 0xab, 0x91}}

	mtMajorType      = guid{0x48eba18e, 0xf8c9, 0x4687, [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	mtSubtype        = guid{0xf7e34c9a, 0x42e8, 0x4714, [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	mtFrameSize      = guid{0x1652c33d, 0xd6b2, 0x4012, [8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d}}
	mtFrameRate      = guid{0xc459a2e8, 0x3d2c, 0x4e44, [8]byte{0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0}}
	mtPixelAspect    = guid{0xc6376a1e, 0x8d0a, 0x4027, [8]byte{0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6}}
	mtInterlaceMode  = guid{0xe2724bb8, 0xe676, 0x4806, [8]byte{0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd}}
	mtAvgBitrate     = guid{0x20332624, 0xfb0d, 0x4d9e, [8]byte{0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e}}
	mtDefaultStride  = guid{0x644b4e48, 0x1e02, 0x4516, [8]byte{0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6}}
	mtProfile        = guid{0xad76a80b, 0x2d5c, 0x4e0b, [8]byte{0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36}}
	mtSequenceHeader = guid{0x3c036de7, 0x3ad0, 0x4c9e, [8]byte{0x92, 0x16, 0xee, 0x6d, 0x6a, 0xc2, 0x1c, 0xb3}}
	mtNominalRange   = guid{0xc21b8ee5, 0xb956, 0x4071, [8]byte{0x8d, 0xaf, 0x32, 0x5e, 0xdf, 0x5c, 0xab, 0x11}}
	mtYUVMatrix      = guid{0x3e23d450, 0x2c75, 0x4d25, [8]byte{0xa0, 0x0e, 0xb9, 0x16, 0x70, 0xd1, 0x23, 0x27}}

	transformAsync       = guid{0xf81a699a, 0x649a, 0x497d, [8]byte{0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a}}
	transformAsyncUnlock = guid{0xe5666d6b, 0x3422, 0x4eb6, [8]byte{0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07}}
	lowLatency           = guid{0x9c27891a, 0xed7a, 0x40e1, [8]byte{0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee}} // also CODECAPI_AVLowLatencyMode
	sampleCleanPoint     = guid{0x9cdf01d8, 0xa0f0, 0x43ba, [8]byte{0xb0, 0x77, 0xea, 0xa0, 0x6c, 0xbd, 0x72, 0x8a}}
	friendlyName         = guid{0x314ffbae, 0x5b41, 0x4c95, [8]byte{0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3}}

	iidTransform      = guid{0xbf94c121, 0x5b05, 0x4e6f, [8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}}
	iidEventGenerator = guid{0x2cd0bd52, 0xbcd5, 0x4b89, [8]byte{0xb6, 0x2c, 0xea, 0xdc, 0x0c, 0x03, 0x1e, 0x7d}}
	iidCodecAPI       = guid{0x901db4c7, 0x31ce, 0x41a2, [8]byte{0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}}

	codecRateControl = guid{0x1c0608e9, 0x370c, 0x4710, [8]byte{0x8a, 0x58, 0xcb, 0x61, 0x81, 0xc4, 0x24, 0x23}}
	codecMeanBitRate = guid{0xf7222374, 0x2144, 0x4815, [8]byte{0xb5, 0x50, 0xa3, 0x7f, 0x8e, 0x12, 0xee, 0x52}}
	codecGOPSize     = guid{0x95f31b26, 0x95a4, 0x41aa, [8]byte{0x93, 0x03, 0x24, 0x6a, 0x7f, 0xc6, 0xee, 0xf1}}
	codecBFrames     = guid{0x8d390aac, 0xdc5c, 0x4200, [8]byte{0xb5, 0x7f, 0x81, 0x4d, 0x04, 0xba, 0xba, 0xb2}}
)

const (
	hrNeedMoreInput = 0xC00D6D72
	hrStreamChange  = 0xC00D6D61
	hrNotAccepting  = 0xC00D36B5

	// IMFAttributes methods (shared by media types, samples, activation objects, events)
	attrGetUINT32          = 7
	attrGetUINT64          = 8
	attrGetGUID            = 10
	attrGetAllocatedString = 13
	attrGetBlobSize        = 14
	attrGetBlob            = 15
	attrSetUINT32          = 21
	attrSetUINT64          = 22
	attrSetGUID            = 24

	// IMFTransform methods
	mftGetStreamIDs           = 5
	mftGetOutputStreamInfo    = 7
	mftGetAttributes          = 8
	mftGetOutputAvailableType = 14
	mftSetInputType           = 15
	mftSetOutputType          = 16
	mftGetOutputCurrentType   = 18
	mftProcessMessage         = 23
	mftProcessInput           = 24
	mftProcessOutput          = 25

	msgFlush            = 0x00000000
	msgBeginStreaming   = 0x10000000
	msgEndStreaming     = 0x10000001
	msgStartOfStream    = 0x10000003
	eventNeedInput      = 601
	eventHaveOutput     = 602
	streamProvidesOwn   = 0x100 | 0x200 // the MFT allocates its own output samples
	enumSync            = 0x01
	enumAsync           = 0x02
	enumHardware        = 0x04
	enumLocal           = 0x08
	enumSortAndFilter   = 0x40
	progressiveScan     = 2
	nominalRange16to235 = 2
	matrixBT709         = 1
)

func codecFormat(codec byte) *guid {
	if codec == proto.CodecH264 {
		return &formatH264
	}
	return &formatHEVC
}

// CodecName is the name people know the codec by.
func CodecName(codec byte) string {
	if codec == proto.CodecH264 {
		return "H.264"
	}
	return "H.265"
}

// call invokes method number idx on a COM object and returns its HRESULT.
func call(obj uintptr, idx int, args ...uintptr) uint32 {
	vtbl := *(*unsafe.Pointer)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return uint32(hr)
}

func ok(hr uint32) bool { return int32(hr) >= 0 }

func release(obj uintptr) {
	if obj != 0 {
		call(obj, 2)
	}
}

func gp(g *guid) uintptr { return uintptr(unsafe.Pointer(g)) }

func setU32(obj uintptr, key *guid, v uint32) uint32 {
	return call(obj, attrSetUINT32, gp(key), uintptr(v))
}
func setU64(obj uintptr, key *guid, v uint64) uint32 {
	return call(obj, attrSetUINT64, gp(key), uintptr(v))
}
func setGUID(obj uintptr, key, v *guid) uint32 { return call(obj, attrSetGUID, gp(key), gp(v)) }

func getU32(obj uintptr, key *guid) (uint32, bool) {
	var v uint32
	hr := call(obj, attrGetUINT32, gp(key), uintptr(unsafe.Pointer(&v)))
	return v, ok(hr)
}

func getU64(obj uintptr, key *guid) (uint64, bool) {
	var v uint64
	hr := call(obj, attrGetUINT64, gp(key), uintptr(unsafe.Pointer(&v)))
	return v, ok(hr)
}

func pair(hi, lo int) uint64 { return uint64(uint32(hi))<<32 | uint64(uint32(lo)) }

var startOnce sync.Once

// initThread prepares the calling (locked) thread for Media Foundation.
func initThread() {
	pCoInitializeEx.Call(0, 0) // multithreaded
	startOnce.Do(func() { pMFStartup.Call(0x00020070, 0) })
}

// worker is a goroutine pinned to one OS thread that runs submitted work in
// order. COM objects created on it are only ever used from it.
type worker struct{ jobs chan func() }

func newWorker() *worker {
	w := &worker{jobs: make(chan func())}
	go func() {
		runtime.LockOSThread()
		initThread()
		for f := range w.jobs {
			f()
		}
	}()
	return w
}

func (w *worker) do(f func()) {
	done := make(chan struct{})
	w.jobs <- func() { f(); close(done) }
	<-done
}

func (w *worker) stop() { close(w.jobs) }

type registerTypeInfo struct{ major, sub guid }

// enumerate lists the MFTs in a category that handle the given types, best
// first, as activation objects the caller must release.
func enumerate(category *guid, flags uint32, in, out *registerTypeInfo) []uintptr {
	var list unsafe.Pointer
	var count uint32
	tail := []uintptr{uintptr(flags), uintptr(unsafe.Pointer(in)), uintptr(unsafe.Pointer(out)),
		uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&count))}
	var args []uintptr
	if runtime.GOARCH == "arm64" {
		// The category GUID is passed by value. On ARM64 a 16-byte
		// structure travels in two registers; on x64 it is passed as a
		// pointer to a copy.
		args = append([]uintptr{uintptr(*(*uint64)(unsafe.Pointer(category))),
			uintptr(*(*uint64)(unsafe.Add(unsafe.Pointer(category), 8)))}, tail...)
	} else {
		g := *category
		args = append([]uintptr{uintptr(unsafe.Pointer(&g))}, tail...)
	}
	hr, _, _ := pMFTEnumEx.Call(args...)
	if !ok(uint32(hr)) || list == nil {
		return nil
	}
	var acts []uintptr
	for _, a := range unsafe.Slice((*uintptr)(list), count) {
		acts = append(acts, a)
	}
	pCoTaskMemFree.Call(uintptr(list))
	return acts
}

func nameOf(activate uintptr) string {
	var p unsafe.Pointer
	var n uint32
	if !ok(call(activate, attrGetAllocatedString, gp(&friendlyName), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n)))) || p == nil {
		return "unnamed"
	}
	s := syscall.UTF16ToString(unsafe.Slice((*uint16)(p), n))
	pCoTaskMemFree.Call(uintptr(p))
	return s
}

// activateMFT turns an activation object into a usable transform.
func activateMFT(activate uintptr) uintptr {
	var mft uintptr
	if !ok(call(activate, 33, gp(&iidTransform), uintptr(unsafe.Pointer(&mft)))) {
		return 0
	}
	return mft
}

func newMediaType() uintptr {
	var t uintptr
	pMFCreateMediaType.Call(uintptr(unsafe.Pointer(&t)))
	return t
}

// newSample makes a sample holding size bytes, which fill writes.
func newSample(size int, fill func(dst []byte), time100ns, duration100ns int64) uintptr {
	var buf, sample uintptr
	if hr, _, _ := pMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&buf))); !ok(uint32(hr)) || buf == 0 {
		return 0
	}
	defer release(buf)
	var p unsafe.Pointer
	if !ok(call(buf, 3, uintptr(unsafe.Pointer(&p)), 0, 0)) || p == nil { // Lock
		return 0
	}
	if fill != nil {
		fill(unsafe.Slice((*byte)(p), size))
	}
	call(buf, 4) // Unlock
	if fill != nil {
		call(buf, 6, uintptr(size)) // SetCurrentLength
	}
	if hr, _, _ := pMFCreateSample.Call(uintptr(unsafe.Pointer(&sample))); !ok(uint32(hr)) || sample == 0 {
		return 0
	}
	call(sample, 42, buf)                // AddBuffer
	call(sample, 36, uintptr(time100ns)) // SetSampleTime
	call(sample, 38, uintptr(duration100ns))
	return sample
}

// withSampleData gives use the sample's bytes as one contiguous block.
func withSampleData(sample uintptr, use func(data []byte)) bool {
	var buf uintptr
	if !ok(call(sample, 41, uintptr(unsafe.Pointer(&buf)))) || buf == 0 { // ConvertToContiguousBuffer
		return false
	}
	defer release(buf)
	var p unsafe.Pointer
	var length uint32
	if !ok(call(buf, 3, uintptr(unsafe.Pointer(&p)), 0, uintptr(unsafe.Pointer(&length)))) || p == nil {
		return false
	}
	use(unsafe.Slice((*byte)(p), length))
	call(buf, 4)
	return true
}

type outputDataBuffer struct {
	streamID uint32
	sample   uintptr
	status   uint32
	events   uintptr
}

type streamInfo struct{ flags, size, alignment uint32 }

const (
	pullGot = iota
	pullNeedInput
	pullStreamChange
	pullFailed
)

// pull asks a transform for one output sample. On pullGot the caller owns
// the returned sample.
func pull(mft uintptr, streamID uint32) (sample uintptr, result int, hr uint32) {
	var info streamInfo
	call(mft, mftGetOutputStreamInfo, uintptr(streamID), uintptr(unsafe.Pointer(&info)))
	ob := outputDataBuffer{streamID: streamID}
	ours := uintptr(0)
	if info.flags&streamProvidesOwn == 0 {
		size := int(info.size)
		if size == 0 {
			size = 8 << 20
		}
		if ours = newSample(size, nil, 0, 0); ours == 0 {
			return 0, pullFailed, 0x8007000E
		}
		ob.sample = ours
	}
	var status uint32
	hr = call(mft, mftProcessOutput, 0, 1, uintptr(unsafe.Pointer(&ob)), uintptr(unsafe.Pointer(&status)))
	release(ob.events)
	if ok(hr) && ob.sample != 0 {
		return ob.sample, pullGot, hr
	}
	if ob.sample != 0 {
		release(ob.sample)
	}
	switch hr {
	case hrNeedMoreInput:
		return 0, pullNeedInput, hr
	case hrStreamChange:
		return 0, pullStreamChange, hr
	}
	return 0, pullFailed, hr
}

var errNone = errors.New("no suitable codec found")

func hrErr(what string, hr uint32) error { return fmt.Errorf("%s: 0x%08X", what, hr) }

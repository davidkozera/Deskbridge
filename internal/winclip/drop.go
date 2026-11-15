//go:build windows

package winclip

// Accepting files dragged onto a window (an OLE drop target).

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	pRegisterDragDrop = ole32.NewProc("RegisterDragDrop")
	pReleaseStgMedium = ole32.NewProc("ReleaseStgMedium")
	iidDropTarget     = guid{0x00000122, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	dropOnce          sync.Once
	dropVtbl          [7]uintptr
	dropMu            sync.Mutex
	dropTargets       = map[uintptr]*dropTarget{} // also keeps them alive: Windows holds only raw pointers
	// These two are handed to other programs by address, so they are kept
	// where their addresses never change. Only the window's thread uses
	// them.
	hdropFormat = formatEtc{cf: cfHDrop, aspect: 1, lindex: -1, tymed: tymedHGlobal}
	dropMedium  stgMedium
)

const (
	dropEffectNone = 0
	dropEffectCopy = 1
)

type dropTarget struct {
	vtbl  uintptr // IDropTarget
	over  func(x, y int) bool
	drop  func(x, y int, files []string)
	files bool // what is being dragged over us right now includes files
}

// RegisterFileDrop makes a window accept files dragged onto it from File
// Explorer and other programs. While files are dragged over the window,
// over is called with the pointer's screen position and says whether they
// may be dropped there; drop is called with the position and the files'
// paths when they are let go. Both run on the window's own thread.
//
// Call it on the thread that owns the window. It reports whether the
// window was registered.
func RegisterFileDrop(hwnd uintptr, over func(x, y int) bool, drop func(x, y int, files []string)) bool {
	if hr, _, _ := pOleInitialize.Call(0); int32(hr) < 0 {
		return false
	}
	dropOnce.Do(func() {
		cb := syscall.NewCallback
		dropVtbl = [7]uintptr{cb(dtQueryInterface), cb(dtAddRef), cb(dtRelease), cb(dtDragEnter), cb(dtDragOver), cb(dtDragLeave), cb(dtDrop)}
	})
	t := &dropTarget{vtbl: uintptr(unsafe.Pointer(&dropVtbl[0])), over: over, drop: drop}
	this := uintptr(unsafe.Pointer(t))
	dropMu.Lock()
	dropTargets[this] = t
	dropMu.Unlock()
	if hr, _, _ := pRegisterDragDrop.Call(hwnd, this); int32(hr) < 0 {
		dropMu.Lock()
		delete(dropTargets, this)
		dropMu.Unlock()
		return false
	}
	return true
}

func getDropTarget(this uintptr) *dropTarget {
	dropMu.Lock()
	defer dropMu.Unlock()
	return dropTargets[this]
}

// comCall calls method number index of a COM object.
func comCall(obj uintptr, index int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	method := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(method, append([]uintptr{obj}, args...)...)
	return r
}

// A data object's methods, by their place in IDataObject.
const (
	dataGetData      = 3
	dataQueryGetData = 5
)

// point unpacks the pointer position, which arrives as two 32-bit numbers
// packed into one argument.
func point(pt uintptr) (x, y int) { return int(int32(pt)), int(int32(pt >> 32)) }

func dtQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return ePointer
	}
	if isGUID(riid, &iidUnknown) || isGUID(riid, &iidDropTarget) {
		putPtr(ppv, this)
		return sOK
	}
	putPtr(ppv, 0)
	return eNoInterface
}

// The object lives as long as the program, so counting references is moot.
func dtAddRef(this uintptr) uintptr  { return 2 }
func dtRelease(this uintptr) uintptr { return 1 }

// answer sets the effect shown by the pointer (a plus sign, or "no entry").
func (t *dropTarget) answer(pt, effect uintptr) uintptr {
	if effect == 0 {
		return ePointer
	}
	e := uint32(dropEffectNone)
	if x, y := point(pt); t.files && t.over(x, y) {
		e = dropEffectCopy
	}
	putU32(effect, e)
	return sOK
}

func dtDragEnter(this, data, keys, pt, effect uintptr) uintptr {
	t := getDropTarget(this)
	if t == nil {
		return eNotImpl
	}
	t.files = data != 0 && comCall(data, dataQueryGetData, uintptr(unsafe.Pointer(&hdropFormat))) == sOK
	return t.answer(pt, effect)
}

func dtDragOver(this, keys, pt, effect uintptr) uintptr {
	t := getDropTarget(this)
	if t == nil {
		return eNotImpl
	}
	return t.answer(pt, effect)
}

func dtDragLeave(this uintptr) uintptr {
	if t := getDropTarget(this); t != nil {
		t.files = false
	}
	return sOK
}

func dtDrop(this, data, keys, pt, effect uintptr) uintptr {
	t := getDropTarget(this)
	if t == nil {
		return eNotImpl
	}
	hr := t.answer(pt, effect)
	accepted := hr == sOK && *(*uint32)(unsafe.Pointer(effect)) == dropEffectCopy
	t.files = false
	if !accepted || data == 0 {
		return hr
	}
	dropMedium = stgMedium{}
	if r := comCall(data, dataGetData, uintptr(unsafe.Pointer(&hdropFormat)), uintptr(unsafe.Pointer(&dropMedium))); int32(r) < 0 || dropMedium.u == 0 {
		putU32(effect, dropEffectNone)
		return sOK
	}
	files := droppedFiles(dropMedium.u)
	pReleaseStgMedium.Call(uintptr(unsafe.Pointer(&dropMedium)))
	if len(files) == 0 {
		putU32(effect, dropEffectNone)
		return sOK
	}
	x, y := point(pt)
	t.drop(x, y, files)
	return sOK
}

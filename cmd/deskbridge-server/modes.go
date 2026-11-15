//go:build windows

package main

// Changing the remote display's resolution.
//
// The client can ask for the display it is watching to be set to any
// resolution that display supports, typically to match the screen the
// person is sitting at. The change is temporary: nothing is written to
// Windows' saved settings, and the display goes back to how it was a minute
// after the client disconnects (a minute, so that a dropped connection that
// comes straight back does not make the display flip twice).

import (
	"encoding/binary"
	"fmt"
	"log"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	pEnumDisplaySettings   = user32.NewProc("EnumDisplaySettingsW")
	pChangeDisplaySettings = user32.NewProc("ChangeDisplaySettingsExW")

	changedMu      sync.Mutex
	changedDevices = map[string]bool{} // displays whose resolution we changed
	restoreTurn    atomic.Int64        // counts disconnections; only the latest one's timer acts
)

// A DEVMODE, the structure Windows describes a display mode with, handled
// as raw bytes at the offsets that matter here.
const (
	devmodeSize   = 220
	dmSizeAt      = 68
	dmFieldsAt    = 72
	dmBitsAt      = 168
	dmWidthAt     = 172
	dmHeightAt    = 176
	dmFrequencyAt = 184

	fieldBits      = 0x00040000
	fieldWidth     = 0x00080000
	fieldHeight    = 0x00100000
	fieldFrequency = 0x00400000
)

func newDevmode() []byte {
	dm := make([]byte, devmodeSize)
	binary.LittleEndian.PutUint16(dm[dmSizeAt:], devmodeSize)
	return dm
}

type mode struct{ w, h, hz int }

// displayModes lists the resolutions a display supports, largest first, each
// with the highest refresh rate available for it, and its current mode.
func displayModes(device string) (current mode, modes []mode) {
	if device == "" {
		return
	}
	name, _ := syscall.UTF16PtrFromString(device)
	dm := newDevmode()
	u32 := func(at int) int { return int(binary.LittleEndian.Uint32(dm[at:])) }
	const currentSettings = 0xFFFFFFFF
	if r, _, _ := pEnumDisplaySettings.Call(uintptr(unsafe.Pointer(name)), currentSettings, uintptr(unsafe.Pointer(&dm[0]))); r != 0 {
		current = mode{u32(dmWidthAt), u32(dmHeightAt), u32(dmFrequencyAt)}
	}
	best := map[[2]int]int{}
	for i := uintptr(0); i < 4096; i++ {
		if r, _, _ := pEnumDisplaySettings.Call(uintptr(unsafe.Pointer(name)), i, uintptr(unsafe.Pointer(&dm[0]))); r == 0 {
			break
		}
		w, h := u32(dmWidthAt), u32(dmHeightAt)
		if u32(dmBitsAt) != 32 || w < 800 || h < 600 || w > 65535 || h > 65535 {
			continue
		}
		if hz := u32(dmFrequencyAt); hz > best[[2]int{w, h}] {
			best[[2]int{w, h}] = hz
		}
	}
	for k, hz := range best {
		modes = append(modes, mode{k[0], k[1], hz})
	}
	sort.Slice(modes, func(i, j int) bool {
		if modes[i].w != modes[j].w {
			return modes[i].w > modes[j].w
		}
		return modes[i].h > modes[j].h
	})
	if len(modes) > 60 {
		modes = modes[:60]
	}
	return current, modes
}

// describeModes is the MsgModes payload for a display: its current size and
// what it could be set to. For "all displays" the list is empty.
func describeModes(device string, w, h int) []byte {
	_, modes := displayModes(device)
	b := binary.LittleEndian.AppendUint16(nil, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	b = append(b, byte(len(modes)))
	for _, m := range modes {
		b = binary.LittleEndian.AppendUint16(b, uint16(m.w))
		b = binary.LittleEndian.AppendUint16(b, uint16(m.h))
	}
	return b
}

// changeResolution sets the shown display to w×h, or back to its own
// setting for 0×0, and tells the client if that was not possible.
func changeResolution(s *session, w, h int) {
	runtime.LockOSThread() // a fresh thread, on the normal desktop
	dev := s.device.Load()
	if dev == nil || *dev == "" {
		tell(s, "The resolution can only be changed while a single display is shown.")
		return
	}
	device := *dev
	name, _ := syscall.UTF16PtrFromString(device)
	if w == 0 && h == 0 {
		changedMu.Lock()
		delete(changedDevices, device)
		changedMu.Unlock()
		pChangeDisplaySettings.Call(uintptr(unsafe.Pointer(name)), 0, 0, 0, 0)
		return
	}
	current, modes := displayModes(device)
	if current.w == w && current.h == h {
		return
	}
	hz := 0
	for _, m := range modes {
		if m.w == w && m.h == h {
			hz = m.hz
		}
	}
	if hz == 0 {
		tell(s, fmt.Sprintf("The remote display does not support %d x %d.", w, h))
		return
	}
	dm := newDevmode()
	binary.LittleEndian.PutUint32(dm[dmBitsAt:], 32)
	binary.LittleEndian.PutUint32(dm[dmWidthAt:], uint32(w))
	binary.LittleEndian.PutUint32(dm[dmHeightAt:], uint32(h))
	binary.LittleEndian.PutUint32(dm[dmFrequencyAt:], uint32(hz))
	binary.LittleEndian.PutUint32(dm[dmFieldsAt:], fieldBits|fieldWidth|fieldHeight|fieldFrequency)
	const testOnly = 2
	apply := func(flags uintptr) int32 {
		r, _, _ := pChangeDisplaySettings.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&dm[0])), 0, flags, 0)
		return int32(r)
	}
	if r := apply(testOnly); r != 0 {
		tell(s, fmt.Sprintf("The remote PC refused %d x %d (code %d).", w, h, r))
		return
	}
	changedMu.Lock()
	changedDevices[device] = true
	changedMu.Unlock()
	if r := apply(0); r != 0 { // 0: for now only, not saved as the PC's setting
		tell(s, fmt.Sprintf("The remote display could not be set to %d x %d (code %d).", w, h, r))
		return
	}
	log.Printf("display %s set to %d x %d at %d Hz for the client", device, w, h, hz)
}

// restoreResolutionsLater puts every display we changed back to its own
// setting, unless a client is connected again a minute from now.
func restoreResolutionsLater() {
	turn := restoreTurn.Add(1)
	go func() {
		time.Sleep(time.Minute)
		if connected() || restoreTurn.Load() != turn {
			return
		}
		runtime.LockOSThread()
		changedMu.Lock()
		defer changedMu.Unlock()
		for device := range changedDevices {
			name, _ := syscall.UTF16PtrFromString(device)
			pChangeDisplaySettings.Call(uintptr(unsafe.Pointer(name)), 0, 0, 0, 0)
			log.Printf("display %s put back to its own resolution", device)
			delete(changedDevices, device)
		}
	}()
}

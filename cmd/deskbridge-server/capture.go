//go:build windows

package main

// Capturing the screen: which part of the desktop, and by which method.
//
// The client chooses one display or all of them. Either way the result is
// one picture (c.pix) covering a rectangle of the desktop; with all displays
// that rectangle spans them, with black wherever no display reaches.
//
// Two methods fill it. The standard one copies the rectangle from the
// screen. The fast one (dda.go) takes each display's frames from the
// graphics card, one duplication per display, and is used whenever it
// works; the standard method is always ready as the fallback.

import (
	"encoding/binary"
	"log"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// monitor is one display's place on the desktop, in pixels. The main
// display's top-left corner is 0,0; others can have negative positions.
type monitor struct {
	x, y, w, h int
	primary    bool
	device     string // Windows' name for it, such as \\.\DISPLAY1
}

var (
	monMu   sync.Mutex
	monList []monitor

	monEnum = syscall.NewCallback(func(hMonitor, hdc, rc, lParam uintptr) uintptr {
		info := struct {
			size    uint32
			monitor [4]int32
			work    [4]int32
			flags   uint32
			device  [32]uint16
		}{size: 104}
		if r, _, _ := pGetMonitorInfo.Call(hMonitor, uintptr(unsafe.Pointer(&info))); r != 0 {
			m := info.monitor
			monList = append(monList, monitor{int(m[0]), int(m[1]), int(m[2] - m[0]), int(m[3] - m[1]), info.flags&1 != 0,
				syscall.UTF16ToString(info.device[:])})
		}
		return 1 // keep going
	})
)

// listMonitors returns the displays, main one first, then left to right.
func listMonitors() []monitor {
	monMu.Lock()
	defer monMu.Unlock()
	monList = nil
	pEnumMonitors.Call(0, 0, monEnum, 0)
	list := monList
	monList = nil
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.primary != b.primary {
			return a.primary
		}
		if a.x != b.x {
			return a.x < b.x
		}
		return a.y < b.y
	})
	return list
}

type capturer struct {
	x, y, w, h                   int       // the rectangle of the desktop being captured
	parts                        []monitor // the displays inside it
	selected                     int       // which display that is; -1 for all
	screenDC, memDC, bmp, oldBmp uintptr
	pix                          []byte

	mons   []monitor // every display, refreshed twice a second
	monsAt time.Time

	// Fast capture: one duplication per display in parts.
	dups     []*dda
	seen     []uint64      // which picture each last gave us (see dda.grab)
	primed   []bool        // which of them have delivered a first picture
	live     bool          // pix is wholly from them, so "nothing changed" can be trusted
	fast     bool          // the last picture came from fast capture
	dupRetry time.Time     // when to next try after a failure
	dupDelay time.Duration // doubles with each failure in a row
	dupNote  string        // last thing logged, to avoid repeats
}

func (c *capturer) dropDups() {
	for _, d := range c.dups {
		d.drop()
	}
	c.dups, c.seen, c.primed, c.live = nil, nil, nil, false
}

func (c *capturer) free() {
	c.dropDups()
	keep := capturer{mons: c.mons, monsAt: c.monsAt, dupRetry: c.dupRetry, dupDelay: c.dupDelay, dupNote: c.dupNote, fast: c.fast, selected: c.selected}
	defer func() { *c = keep }()
	if c.memDC != 0 {
		pSelectObject.Call(c.memDC, c.oldBmp)
		pDeleteObject.Call(c.bmp)
		pDeleteDC.Call(c.memDC)
		pReleaseDC.Call(0, c.screenDC)
	}
}

// describe is the display list in the form sent to the client: which one is
// selected, then each display's position, size and whether it is the main one.
func (c *capturer) describe() []byte {
	b := []byte{byte(int8(c.selected)), byte(min(len(c.mons), 16))}
	for i, m := range c.mons {
		if i >= 16 {
			break
		}
		b = binary.LittleEndian.AppendUint32(b, uint32(int32(m.x)))
		b = binary.LittleEndian.AppendUint32(b, uint32(int32(m.y)))
		b = binary.LittleEndian.AppendUint32(b, uint32(m.w))
		b = binary.LittleEndian.AppendUint32(b, uint32(m.h))
		flag := byte(0)
		if m.primary {
			flag = 1
		}
		b = append(b, flag)
	}
	return b
}

// ensure sets up capturing for the chosen display (or all, for a negative
// choice), again whenever the choice, the display layout or a resolution
// changes. changed reports that the picture size or place is new.
func (c *capturer) ensure(choice int) (changed bool, ok bool) {
	if now := time.Now(); c.mons == nil || now.Sub(c.monsAt) > 500*time.Millisecond {
		c.mons, c.monsAt = listMonitors(), now
	}
	if len(c.mons) == 0 {
		// Could not list displays: fall back to the main one's size.
		w, _, _ := pGetSystemMetrics.Call(0)
		h, _, _ := pGetSystemMetrics.Call(1)
		c.mons = []monitor{{0, 0, int(w), int(h), true, ""}}
	}
	var parts []monitor
	var x, y, w, h int
	if choice < 0 && len(c.mons) > 1 {
		c.selected = -1
		parts = c.mons
		x1, y1 := c.mons[0].x+c.mons[0].w, c.mons[0].y+c.mons[0].h
		x, y = c.mons[0].x, c.mons[0].y
		for _, m := range c.mons[1:] {
			x, y = min(x, m.x), min(y, m.y)
			x1, y1 = max(x1, m.x+m.w), max(y1, m.y+m.h)
		}
		w, h = x1-x, y1-y
	} else {
		if choice < 0 || choice >= len(c.mons) {
			choice = 0
		}
		c.selected = choice
		m := c.mons[choice]
		parts, x, y, w, h = []monitor{m}, m.x, m.y, m.w, m.h
	}
	if x == c.x && y == c.y && w == c.w && h == c.h && len(parts) == len(c.parts) && c.memDC != 0 {
		return false, true
	}
	c.free()
	if w <= 0 || h <= 0 || w > 65535 || h > 65535 {
		return false, false
	}
	screenDC, _, _ := pGetDC.Call(0)
	if screenDC == 0 {
		return false, false
	}
	memDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	bi := bitmapInfoHeader{Size: 40, Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	var bits unsafe.Pointer
	bmp, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if memDC == 0 || bmp == 0 || bits == nil {
		if bmp != 0 {
			pDeleteObject.Call(bmp)
		}
		if memDC != 0 {
			pDeleteDC.Call(memDC)
		}
		pReleaseDC.Call(0, screenDC)
		return false, false
	}
	old, _, _ := pSelectObject.Call(memDC, bmp)
	c.x, c.y, c.w, c.h, c.parts = x, y, w, h, parts
	c.screenDC, c.memDC, c.bmp, c.oldBmp = screenDC, memDC, bmp, old
	c.pix = unsafe.Slice((*byte)(bits), w*h*4)
	return true, true
}

// grab captures into c.pix. With fast it uses Desktop Duplication when that
// works, waiting up to waitMs for the screen to change; same then reports
// that it has not, and c.pix is as it was. Otherwise, and whenever fast
// capture is unavailable, it copies the screen the standard way.
func (c *capturer) grab(fast bool, waitMs int) (same, ok bool) {
	now := time.Now()
	if !fast {
		c.dropDups()
	} else if c.dups == nil && now.After(c.dupRetry) {
		var ds []*dda
		var err error
		for _, m := range c.parts {
			var d *dda
			if d, err = acquireDDA(m.x, m.y); err != nil {
				break
			}
			ds = append(ds, d)
		}
		if err != nil {
			for _, d := range ds {
				d.drop()
			}
			c.dupDelay = min(max(c.dupDelay*2, time.Second), time.Minute)
			c.dupRetry = now.Add(c.dupDelay)
			if note := err.Error(); note != c.dupNote {
				c.dupNote = note
				log.Printf("capture: fast capture unavailable, using the standard method: %v", err)
			}
		} else {
			c.dups, c.seen, c.primed = ds, make([]uint64, len(ds)), make([]bool, len(ds))
			if c.dupNote != "fast" {
				c.dupNote = "fast"
				log.Print("capture: using fast capture (Desktop Duplication)")
			}
		}
	}
	if c.dups != nil {
		frame, lost, ready := false, false, true
		for i, d := range c.dups {
			m := c.parts[i]
			// Wait for a change on the first display only, so the others
			// do not add delay; but give each time for its first picture.
			wait := 0
			if i == 0 {
				wait = waitMs
			}
			if !c.primed[i] {
				wait = max(wait, 100)
			}
			switch d.grab(&c.seen[i], c.pix, c.w, m.x-c.x, m.y-c.y, m.w, m.h, wait) {
			case ddaFrame:
				frame, c.primed[i] = true, true
			case ddaSame:
			default:
				lost = true
			}
			if lost {
				break
			}
			ready = ready && c.primed[i]
		}
		switch {
		case lost:
			// Normal when the desktop or display mode changes. Make new
			// ones shortly, backing off if it keeps happening.
			c.dropDups()
			c.dupDelay = min(max(c.dupDelay*2, 250*time.Millisecond), time.Minute)
			c.dupRetry = now.Add(c.dupDelay)
		case ready:
			c.live, c.fast, c.dupDelay = true, true, 0
			return !frame, true
		}
		// Not every display has delivered a picture yet: use the standard
		// method meanwhile.
	}
	// SRCCOPY | CAPTUREBLT, from the rectangle's place on the desktop.
	r, _, _ := pBitBlt.Call(c.memDC, 0, 0, uintptr(c.w), uintptr(c.h), c.screenDC, uintptr(c.x), uintptr(c.y), 0x00CC0020|0x40000000)
	pGdiFlush.Call()
	c.live, c.fast = false, false
	return false, r != 0
}

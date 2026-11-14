//go:build windows

package main

// Two pane controls that act on the remote PC: the display picker, and the
// "Send to remote PC" menu for things that cannot be typed across
// (Ctrl+Alt+Del, locking) or are not keys at all (sign out, restart).

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"unsafe"

	"deskbridge/internal/proto"
)

const (
	wmMonitors = wmApp + 15 // main: the server sent its list of displays

	idMonitor = 112
	idSend    = 113

	noPreference = -2 // no display has been chosen by hand yet
)

var (
	pCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	pAppendMenu      = user32.NewProc("AppendMenuW")
	pTrackPopupMenu  = user32.NewProc("TrackPopupMenu")
	pDestroyMenu     = user32.NewProc("DestroyMenu")
	pMessageBoxW     = user32.NewProc("MessageBoxW")

	remoteDisplays atomic.Pointer[displayList] // the latest list from the server

	// Touched only on the UI thread.
	hMonitor, hSend  uintptr
	displays         displayList // what the picker currently shows
	wantedDisplay    = noPreference
	displayRequested bool // the saved choice has been asked for on this connection
)

type display struct {
	x, y, w, h int
	main       bool
}

type displayList struct {
	selected int // index into list, or -1 for all
	list     []display
}

// parseDisplays reads a MsgMonitors payload.
func parseDisplays(p []byte) *displayList {
	if len(p) < 2 || len(p) < 2+17*int(p[1]) {
		return nil
	}
	d := &displayList{selected: int(int8(p[0]))}
	for i := 0; i < int(p[1]); i++ {
		q := p[2+17*i:]
		d.list = append(d.list, display{
			x: int(int32(binary.LittleEndian.Uint32(q))), y: int(int32(binary.LittleEndian.Uint32(q[4:]))),
			w: int(binary.LittleEndian.Uint32(q[8:])), h: int(binary.LittleEndian.Uint32(q[12:])), main: q[16]&1 != 0,
		})
	}
	return d
}

// showDisplays fills the picker from the server's list. The last entry,
// when there is more than one display, shows them all side by side.
func showDisplays() {
	d := remoteDisplays.Load()
	if d == nil || cur.Load() == nil {
		displays = displayList{}
		pSendMessage.Call(hMonitor, 0x014B, 0, 0) // CB_RESETCONTENT
		pEnableWindow.Call(hMonitor, 0)
		return
	}
	displays = *d
	pSendMessage.Call(hMonitor, 0x014B, 0, 0)
	add := func(s string) { pSendMessage.Call(hMonitor, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr(s)))) }
	x0, y0, x1, y1 := 0, 0, 0, 0
	for i, m := range d.list {
		name := fmt.Sprintf("Display %d:  %d x %d", i+1, m.w, m.h)
		if m.main {
			name += "  (main)"
		}
		add(name)
		if i == 0 {
			x0, y0, x1, y1 = m.x, m.y, m.x+m.w, m.y+m.h
		}
		x0, y0, x1, y1 = min(x0, m.x), min(y0, m.y), max(x1, m.x+m.w), max(y1, m.y+m.h)
	}
	if len(d.list) > 1 {
		add(fmt.Sprintf("All displays:  %d x %d", x1-x0, y1-y0))
	}
	index := d.selected
	if index < 0 {
		index = len(d.list) // the "all" entry
	}
	pSendMessage.Call(hMonitor, 0x014E, uintptr(index), 0) // CB_SETCURSEL
	pEnableWindow.Call(hMonitor, 1)

	// Go back to the display chosen last time, once per connection, if the
	// remote PC still has it.
	if !displayRequested && wantedDisplay != noPreference {
		displayRequested = true
		valid := wantedDisplay < len(d.list) && (wantedDisplay >= 0 || len(d.list) > 1)
		if valid && wantedDisplay != d.selected {
			queue(proto.MsgSelectMonitor, []byte{byte(int8(wantedDisplay))})
		}
	}
}

// displayPicked runs when the person chooses an entry.
func displayPicked() {
	i, _, _ := pSendMessage.Call(hMonitor, 0x0147, 0, 0) // CB_GETCURSEL
	index := int(int32(i))
	if index < 0 || index > len(displays.list) {
		return
	}
	if index == len(displays.list) {
		index = -1 // all
	}
	wantedDisplay, displayRequested = index, true
	queue(proto.MsgSelectMonitor, []byte{byte(int8(index))})
	saveSettings()
	pSetFocus.Call(hView)
}

// sendMenu shows the "Send to remote PC" menu under its button and carries
// out the choice.
func sendMenu() {
	if cur.Load() == nil {
		return
	}
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	item := func(id uintptr, text string) { pAppendMenu.Call(menu, 0, id, uintptr(unsafe.Pointer(u16ptr(text)))) }
	separator := func() { pAppendMenu.Call(menu, 0x0800, 0, 0) }
	item(proto.ActionCtrlAltDel, "Ctrl+Alt+Del\tCtrl+Alt+End")
	item(proto.ActionLock, "Lock")
	item(proto.ActionTaskManager, "Task Manager")
	separator()
	item(proto.ActionSignOut, "Sign out...")
	item(proto.ActionRestart, "Restart...")
	item(proto.ActionShutDown, "Shut down...")

	var rc rect
	pGetWindowRect.Call(hSend, uintptr(unsafe.Pointer(&rc)))
	// TPM_RETURNCMD | TPM_NONOTIFY: just tell us what was picked.
	choice, _, _ := pTrackPopupMenu.Call(menu, 0x0100|0x0080, uintptr(rc.left), uintptr(rc.bottom), 0, hMain, 0)
	if choice == 0 {
		return
	}
	question := map[uintptr]string{
		proto.ActionSignOut:  "Sign out of the remote PC? Programs open there will be closed.",
		proto.ActionRestart:  "Restart the remote PC? You will be disconnected; Deskbridge starts again with it.",
		proto.ActionShutDown: "Shut down the remote PC? You will not be able to reach it again until someone turns it on.",
	}[choice]
	if question != "" {
		const yesNo, questionIcon, defaultNo, yes = 0x04, 0x20, 0x100, 6
		if r, _, _ := pMessageBoxW.Call(hMain, uintptr(unsafe.Pointer(u16ptr(question))),
			uintptr(unsafe.Pointer(u16ptr("Deskbridge"))), yesNo|questionIcon|defaultNo); r != yes {
			return
		}
	}
	queue(proto.MsgAction, []byte{byte(choice)})
	pSetFocus.Call(hView)
}

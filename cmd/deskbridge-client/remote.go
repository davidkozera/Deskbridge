//go:build windows

package main

// Two pane controls that act on the remote PC: the display picker, and the
// "Send to remote PC" menu for things that cannot be typed across
// (Ctrl+Alt+Del, locking) or are not keys at all (sign out, restart).

import (
	"encoding/binary"
	"fmt"
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

	// Touched only on the UI thread.
	hMonitor, hSend uintptr
	displays        displayList // what the picker currently shows
	// wantedDisplay is the display the connection being set up should
	// show, while no tab is open. Each tab has its own.
	wantedDisplay = noPreference
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

// showDisplays fills the picker from the list sent by the server of the
// tab in front. The last entry, when there is more than one display, shows
// them all side by side.
func showDisplays() {
	var d *displayList
	if l := cur.Load(); l != nil {
		d = l.displays.Load()
	}
	if d == nil {
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
}

// requestDisplay has a tab's connection go back to the display chosen for
// it last time, once per connection, if the remote PC still has it. It
// applies to tabs behind the one in front too.
func requestDisplay(tb *tab) {
	if tb.l == nil || tb.displayRequested || tb.wantedDisplay == noPreference {
		return
	}
	d := tb.l.displays.Load()
	if d == nil {
		return
	}
	tb.displayRequested = true
	valid := tb.wantedDisplay < len(d.list) && (tb.wantedDisplay >= 0 || len(d.list) > 1)
	if valid && tb.wantedDisplay != d.selected {
		sendTo(tb.l, proto.MsgSelectMonitor, []byte{byte(int8(tb.wantedDisplay))})
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
	tb := active
	if tb == nil || tb.l == nil {
		return
	}
	tb.wantedDisplay, tb.displayRequested = index, true
	if p := currentProfile(); p >= 0 {
		computers[p].display = index
	}
	// The connection's resolution goes with it to the display now chosen.
	tb.resRequested = false
	queue(proto.MsgSelectMonitor, []byte{byte(int8(index))})
	saveSettings()
	pSetFocus.Call(hView)
}

// sendMenu shows the "Send to remote PC" menu under its button in the pane.
func sendMenu() {
	var rc rect
	pGetWindowRect.Call(hSend, uintptr(unsafe.Pointer(&rc)))
	sendMenuAt(rc.left, rc.bottom)
}

// sendMenuAt shows the "Send to remote PC" menu at a place on the screen
// and carries out the choice.
func sendMenuAt(x, y int32) {
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

	// TPM_RETURNCMD | TPM_NONOTIFY: just tell us what was picked.
	choice, _, _ := pTrackPopupMenu.Call(menu, 0x0100|0x0080, uintptr(x), uintptr(y), 0, hMain, 0)
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

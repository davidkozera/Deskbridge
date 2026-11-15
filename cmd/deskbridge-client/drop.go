//go:build windows

package main

// Dragging files from this PC onto the remote screen.
//
// Dropped files travel the same way as copied ones (see internal/winclip):
// they are offered to the remote PC, appear on its clipboard, and cross
// when something there pastes them, with Windows' own copy progress
// window. To make that feel like a drop, once the remote PC confirms it
// has them the client clicks where they were dropped, so that window or
// folder is the one in use, and presses Ctrl+V there.
//
// A server older than 1.7.0 does not confirm. The files still reach its
// clipboard; the person is told to press Ctrl+V themselves.

import (
	"strconv"
	"unsafe"

	"deskbridge/internal/proto"
	"deskbridge/internal/update"
	"deskbridge/internal/winclip"
)

const (
	// main: the outcome of a drop; wParam is its id, lParam 1 if the remote
	// PC has the files on its clipboard, 0 if they could not be sent.
	wmDropShown = wmApp + 21

	timerDropHint  = 3 // the remote PC has been slow to confirm: say how to paste by hand
	timerDropPaste = 4 // the click has had time to take effect: press Ctrl+V
)

// pendingDrop is the drop being carried out. Touched only on the UI thread.
var pendingDrop struct {
	id    uint32  // 0: none
	link  uintptr // the connection it was dropped on
	x, y  int     // where, on the remote screen
	count int
}

func setupFileDrop() {
	winclip.RegisterFileDrop(hView, dragOver, filesDropped)
}

// viewPoint turns a screen position into one on the remote screen.
func viewPoint(sx, sy int) (x, y int, ok bool) {
	pt := [2]int32{int32(sx), int32(sy)}
	pScreenToClient.Call(hView, uintptr(unsafe.Pointer(&pt)))
	return remoteAt(int(pt[0]), int(pt[1]))
}

// dragOver runs while files are dragged over the remote screen. It moves
// the remote pointer along, so it is plain where they would land, and says
// whether they can be dropped.
func dragOver(sx, sy int) bool {
	if cur.Load() == nil {
		return false
	}
	x, y, ok := viewPoint(sx, sy)
	if ok {
		queue(proto.MsgMouseMove, xy(x, y))
	}
	return ok
}

func items(n int) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.Itoa(n) + " items"
}

// filesDropped runs when files are let go over the remote screen.
func filesDropped(sx, sy int, files []string) {
	l := cur.Load()
	x, y, ok := viewPoint(sx, sy)
	if l == nil || !ok {
		return
	}
	id := uint32(0)
	if clip != nil {
		id = clip.OfferFiles(files)
	}
	if id == 0 {
		setStatus("The files were not sent: dropping files uses copy and paste. Tick \"Share copy and paste\" and try again.")
		openPane()
		return
	}
	pKillTimer.Call(hMain, timerDropPaste)
	pKillTimer.Call(hMain, timerDropHint)
	// Only a server from 1.7.0 on says when the files are ready to paste.
	confirms := false
	if v := l.version.Load(); v != nil {
		confirms = *v == proto.Version || !update.Newer("1.7.0", *v)
	}
	if !confirms {
		pendingDrop.id = 0
		setStatus("Sending " + items(len(files)) + " to the remote PC's clipboard. Give it a moment, then click where you want them there and press Ctrl+V.\r\n\r\nOnce the remote PC is updated to 1.7.0, dropped files are pasted where you drop them.")
		return
	}
	pendingDrop.id, pendingDrop.link, pendingDrop.x, pendingDrop.y, pendingDrop.count = id, l.id, x, y, len(files)
	pSetTimer.Call(hMain, timerDropHint, 20000, 0)
	setStatus("Sending " + items(len(files)) + " to the remote PC...")
}

// dropShown is called by clipboard sharing, on its own goroutine, with the
// outcome of a drop.
func dropShown(id uint32, ok bool) {
	sent := uintptr(0)
	if ok {
		sent = 1
	}
	pPostMessage.Call(hMain, wmDropShown, uintptr(id), sent)
}

// dropStillValid reports whether the pending drop can go ahead: it is the
// one meant and its connection is still the one in front.
func dropStillValid(id uint32) bool {
	l := cur.Load()
	return pendingDrop.id != 0 && pendingDrop.id == id && l != nil && l.id == pendingDrop.link
}

// dropConfirmed runs when the outcome of a drop is known. If the remote PC
// has the files on its clipboard: click where they were dropped, and paste
// a moment later.
func dropConfirmed(id uint32, sent bool) {
	if !sent {
		if pendingDrop.id == id { // the reason is already in the pane
			pendingDrop.id = 0
			pKillTimer.Call(hMain, timerDropHint)
		}
		return
	}
	if !dropStillValid(id) {
		return
	}
	pKillTimer.Call(hMain, timerDropHint)
	at := xy(pendingDrop.x, pendingDrop.y)
	queue(proto.MsgMouseMove, at)
	queue(proto.MsgMouseBtn, append([]byte{0, 1}, at...))
	queue(proto.MsgMouseBtn, append([]byte{0, 0}, at...))
	// The click makes that window the one in use, which takes the remote
	// PC a moment; Ctrl+V sent at once could land in the window before.
	pSetTimer.Call(hMain, timerDropPaste, 350, 0)
}

func dropTimer(timer uintptr) {
	pKillTimer.Call(hMain, timer)
	id, count := pendingDrop.id, pendingDrop.count
	if timer == timerDropHint {
		// No word from the remote PC for a long while: a very large folder
		// still being listed, perhaps. Leave the pasting to the person.
		if id != 0 {
			pendingDrop.id = 0
			setStatus("The dropped files (" + items(count) + ") are still on their way to the remote PC's clipboard. When they are there, click where you want them and press Ctrl+V.")
		}
		return
	}
	if !dropStillValid(id) {
		pendingDrop.id = 0
		return
	}
	pendingDrop.id = 0
	const vkLCtrl, scanLCtrl, vkV, scanV = 0xA2, 0x1D, 0x56, 0x2F
	key := func(vk, scan uint16, up bool) {
		p := []byte{byte(vk), byte(vk >> 8), byte(scan), byte(scan >> 8), 0}
		if up {
			p[4] = 2
		}
		queue(proto.MsgKey, p)
	}
	key(vkLCtrl, scanLCtrl, false)
	key(vkV, scanV, false)
	key(vkV, scanV, true)
	key(vkLCtrl, scanLCtrl, true)
	setStatus("Pasting " + items(count) + " on the remote PC where you dropped them. Windows shows its copy progress there.")
}

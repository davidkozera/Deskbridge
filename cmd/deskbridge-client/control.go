//go:build windows

package main

// Sharing a remote PC with other PCs connected to it.
//
// A server from version 1.8.0 on serves several PCs at once. They all see
// its screen; one of them at a time has its mouse and keyboard. A PC takes
// control by clicking, typing or scrolling on the remote screen, or with
// the Take control button that appears over the remote screen while
// another PC has it. While watching, the remote pointer is drawn on the
// picture, and copy and paste is not shared.

import (
	"deskbridge/internal/proto"
	"deskbridge/internal/winclip"
)

const (
	wmControl = wmApp + 22 // main: a connection gained or lost control; wParam is its id

	idTake = 119
)

var (
	pDrawIconEx = user32.NewProc("DrawIconEx")

	hTake uintptr // the Take control button
)

// watchingNote is the pane's explanation while another PC is in control.
func watchingNote(l *link) string {
	who := "Another PC"
	if name := l.controller.Load(); name != nil && *name != "" {
		who += " (" + *name + ")"
	}
	return who + " is in control of this remote PC, so you are watching. Click or type on the remote screen, or click Take control, to take over."
}

// showTakeControl shows the button over the remote screen exactly while
// the connection in front is being watched rather than controlled.
func showTakeControl() {
	show := uintptr(0)
	if l := cur.Load(); l != nil && !l.inControl.Load() {
		show = 8 // SW_SHOWNA
		const hwndTop, noMove, noSize, noActivate = 0, 0x2, 0x1, 0x10
		pSetWindowPos.Call(hTake, hwndTop, 0, 0, 0, 0, noMove|noSize|noActivate)
	}
	pShowWindow.Call(hTake, show)
}

func takeControlClicked() {
	queue(proto.MsgTakeControl, nil)
	pSetFocus.Call(hView)
}

// controlChanged runs when a tab's server has said which PC is in control.
func controlChanged(tb *tab) {
	l := tb.l
	watching := !l.inControl.Load()
	switch {
	case watching:
		if !tb.watching {
			// From here on, note whether something gets copied on this PC.
			l.clipSeq.Store(winclip.Sequence())
		}
		setTabStatus(tb, watchingNote(l))
	case tb.watching:
		setTabStatus(tb, "You are in control of "+tb.target.name+" again.")
		if tb == active {
			// Something copied here while watching was not sent: send it
			// now, so it can be pasted there.
			if l.peer != nil && clipOn.Load() && winclip.Sequence() != l.clipSeq.Load() {
				l.peer.Announce()
			}
			// Files dropped while watching were waiting for this.
			if heldDrop.link == l.id && len(heldDrop.files) > 0 {
				sx, sy, files := heldDrop.sx, heldDrop.sy, heldDrop.files
				heldDrop.files = nil
				filesDropped(sx, sy, files)
			}
		}
	}
	tb.watching = watching
	if tb == active {
		showTakeControl()
		pInvalidateRect.Call(hView, 0, 0)
	}
	pInvalidateRect.Call(hTabs, 0, 0)
}

//go:build windows

package main

// Several clients at once, and which of them is in control.
//
// Every connected client sees the PC's one real screen (each may look at
// a different display). The mouse and keyboard, though, belong to one
// client at a time: the one "in control". A client takes control by
// clicking, typing or scrolling on the remote screen, by one of the "Send
// to remote PC" actions, or by asking for it outright (its Take Control
// button). Moving the pointer alone does not, so that someone merely
// looking is not fought over. Copy and paste is shared with the client in
// control; sound goes to every client that wants it.

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
	"deskbridge/internal/winclip"
)

// maxClients bounds how many clients are served at once. One more than
// that takes the place of the client that has been connected longest
// without being in control.
const maxClients = 8

var (
	sessMu   sync.Mutex
	sessions []*session              // every connected client, oldest first
	control  atomic.Pointer[session] // the one in control; nil if none is
)

// connected reports whether any client is connected.
func connected() bool {
	sessMu.Lock()
	defer sessMu.Unlock()
	return len(sessions) > 0
}

func inControl(s *session) bool { return control.Load() == s }

// addSession makes a newly connected client one of those being served. The
// first one to connect is in control straight away.
func addSession(s *session) {
	sessMu.Lock()
	// A client reconnecting after its network dropped may still have its
	// old connection on the books here, which nothing has been heard from
	// since. That one is finished: the new connection takes its place,
	// and its control if it had it, instead of waiting it out as a
	// watcher of itself.
	var stale []*session
	for _, o := range sessions {
		if o.remote == s.remote && o.pinged.Load() && time.Since(time.Unix(0, o.heard.Load())) > 4*time.Second {
			stale = append(stale, o)
			if inControl(o) {
				control.Store(nil)
			}
		}
	}
	var replaced *session
	if len(sessions) >= maxClients {
		for _, o := range sessions {
			if !inControl(o) {
				replaced = o
				break
			}
		}
		if replaced == nil {
			replaced = sessions[0]
		}
	}
	sessions = append(sessions, s)
	first := control.Load() == nil
	if first {
		control.Store(s)
	}
	others := len(sessions) - 1
	sessMu.Unlock()

	for _, o := range stale {
		o.close() // its own goroutine then takes it off the books
	}
	if replaced != nil {
		// Tell it why, so it does not reconnect and start a tug of war.
		replaced.c.Send(proto.MsgNotice, true, []byte{proto.NoticeReplaced, '.'})
		replaced.close()
	}
	if first {
		clipControl(s)
	}
	announceControl()
	if others > 0 {
		for _, o := range allSessions() {
			if o != s {
				tell(o, "Another PC ("+s.remote+") has connected to this PC as well.")
			}
		}
	}
}

// removeSession forgets a client that has gone. If it was in control and
// exactly one client is left, that one takes over; with several left,
// control waits for whichever of them acts first.
func removeSession(s *session) {
	sessMu.Lock()
	for i, o := range sessions {
		if o == s {
			sessions = append(sessions[:i], sessions[i+1:]...)
			break
		}
	}
	var heir *session
	wasControl := control.Load() == s
	if wasControl {
		if len(sessions) == 1 {
			heir = sessions[0]
		}
		control.Store(heir)
	}
	sessMu.Unlock()
	if wasControl {
		clipControl(heir)
		announceControl()
	}
}

func allSessions() []*session {
	sessMu.Lock()
	defer sessMu.Unlock()
	return append([]*session(nil), sessions...)
}

// takeControl gives a client the mouse and keyboard. Whatever the client
// that had them was holding down is let go first, so nothing is left
// stuck. It must run on a thread that can send input (an input loop's).
func takeControl(s *session) {
	if inControl(s) {
		return
	}
	sessMu.Lock()
	old := control.Load()
	present := false
	for _, o := range sessions {
		present = present || o == s
	}
	if old == s || !present {
		sessMu.Unlock()
		return
	}
	control.Store(s)
	sessMu.Unlock()
	if old != nil {
		old.releaseHeld()
	}
	log.Printf("%s: took control", s.remote)
	clipControl(s)
	announceControl()
}

// announceControl tells every client whether it is the one in control, and
// if not, which PC is.
func announceControl() {
	who := control.Load()
	for _, o := range allSessions() {
		msg := []byte{0}
		if o == who {
			msg[0] = 1
		} else if who != nil {
			msg = append(msg, who.remote...)
		}
		o.c.Send(proto.MsgControl, true, msg)
	}
}

// clipControl points copy and paste at the client now in control (nil: at
// nobody). Nothing is pushed to it at that moment: a client that copied
// something while it was only watching sends that itself when it takes
// control, and pushing this PC's clipboard the other way at the same time
// would have the two overwrite each other.
func clipControl(s *session) {
	switch {
	case workerMode:
		clipRelayState(clipOff) // the helper starts from a clean slate
		if s != nil && s.clipOn.Load() {
			clipRelayState(clipOn)
		}
	case s != nil:
		if p, ok := s.peer.(*winclip.Peer); ok {
			p.Activate(false)
		}
	}
}

// hostOf is the address without its port, for naming a client in messages.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// ---------- what a client is holding down ----------

// keyDown and keyUp keep track of the keys a client has pressed, by the
// event that releases each.
func (s *session) keyDown(id uint32, release keybdInput) {
	s.heldMu.Lock()
	if s.heldKeys == nil {
		s.heldKeys = map[uint32]keybdInput{}
	}
	s.heldKeys[id] = release
	s.heldMu.Unlock()
}

func (s *session) keyUp(id uint32) {
	s.heldMu.Lock()
	delete(s.heldKeys, id)
	s.heldMu.Unlock()
}

// buttonChange notes a mouse button going down or up at (x, y).
func (s *session) buttonChange(button byte, down bool, x, y int) {
	s.heldMu.Lock()
	if down {
		s.heldButtons |= 1 << button
	} else {
		s.heldButtons &^= 1 << button
	}
	s.lastX, s.lastY = x, y
	s.heldMu.Unlock()
}

// The SendInput flags for each mouse button going down and up.
var buttonFlags = [5][2]uint32{{0x4, 0x2}, {0x10, 0x8}, {0x40, 0x20}, {0x100, 0x80}, {0x100, 0x80}}

func buttonData(button byte) uint32 {
	if button >= 3 {
		return uint32(button) - 2 // XBUTTON1 / XBUTTON2
	}
	return 0
}

// releaseHeld lets go of every key and mouse button the client still has
// down on this PC. It runs on the calling thread, which must be able to
// send input.
func (s *session) releaseHeld() {
	s.heldMu.Lock()
	keys, buttons, x, y := s.heldKeys, s.heldButtons, s.lastX, s.lastY
	s.heldKeys, s.heldButtons = nil, 0
	s.heldMu.Unlock()
	for _, in := range keys {
		pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	}
	for b := byte(0); b < 5; b++ {
		if buttons&(1<<b) != 0 {
			sendMouse(s, x, y, buttonFlags[b][0], buttonData(b))
		}
	}
}

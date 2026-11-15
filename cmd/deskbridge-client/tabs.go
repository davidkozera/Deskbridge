//go:build windows

package main

// Several connections at once, each in a tab.
//
// A tab is one connection the person has opened: it lasts from the click on
// Connect until it is closed, through connecting, being connected, and any
// reconnecting in between. One tab is in front. Its remote screen is the
// one on show, it gets the keyboard, mouse, sound and clipboard, and the
// pane's display and resolution lists are about it. The others stay
// connected behind it at a low frame rate, so their pictures are current
// the moment they are switched to.
//
// The strip of tabs sits above the remote screen, each tab labelled with
// its connection's name. Click a tab to switch to it and its x (or
// middle-click it) to close it; Ctrl+Alt+PageDown and Ctrl+Alt+PageUp
// step through them, which also works in full screen.

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

const (
	wmNameChanged = wmApp + 19 // main: the connection boxes changed; the Connect button may need relabelling
	wmSwitchTab   = wmApp + 20 // main: step through the tabs; wParam 1 for the next, 0 for the previous

	tabsHeight = 30 // at 96 dpi
	maxTabs    = 12

	timerReconnectBase = 1000 // a reconnect timer's id is this plus its tab's id
)

var (
	pFillRect         = user32.NewProc("FillRect")
	pGetSysColor      = user32.NewProc("GetSysColor")
	pGetSysColorBrush = user32.NewProc("GetSysColorBrush")
)

// tab is one opened connection. Touched only on the UI thread.
type tab struct {
	id       uintptr
	target   target
	remember bool  // "Remember password" as it stood when the connection was started
	l        *link // the live connection; nil while connecting, reconnecting or stopped

	connecting        bool // an attempt is under way
	reconnecting      bool // attempts keep being made until one works
	attempt           int
	everConnected     bool
	restoreFullscreen bool   // go back to full screen once reconnected
	viewNote          string // shown in place of the remote screen while there is none
	status            string // this tab's text for the pane
	watching          bool   // another PC was in control when we last heard

	opts options // its frame rate, quality and tick boxes

	wantedDisplay    int        // noPreference, -1 for all, or a display's index
	displayRequested bool       // it has been asked for on this connection
	wantedRes        resolution // zero: leave the remote display as it is
	resRequested     bool
}

// stopped reports that nothing is connected or being tried: the attempt
// failed, or another PC took the connection over.
func (tb *tab) stopped() bool { return tb.l == nil && !tb.connecting && !tb.reconnecting }

var (
	// Touched only on the UI thread.
	hTabs     uintptr
	tabs      []*tab
	active    *tab // the tab in front; nil when there are none
	nextTabID uintptr

	connectResults = make(chan *connectResult, 64)
)

func tabByID(id uintptr) *tab {
	for _, tb := range tabs {
		if tb.id == id {
			return tb
		}
	}
	return nil
}

// tabOfLink finds the tab a live connection belongs to.
func tabOfLink(id uintptr) *tab {
	for _, tb := range tabs {
		if tb.l != nil && tb.l.id == id {
			return tb
		}
	}
	return nil
}

func tabNamed(name string) *tab {
	for _, tb := range tabs {
		if name != "" && strings.EqualFold(tb.target.name, name) {
			return tb
		}
	}
	return nil
}

// tabFor finds the tab already open for a connection: one of the same
// name, or one to the same PC (a second tab to it would only show the same
// screen twice).
func tabFor(t target) *tab {
	if tb := tabNamed(t.name); tb != nil {
		return tb
	}
	for _, tb := range tabs {
		if strings.EqualFold(tb.target.host, t.host) && tb.target.port == t.port && !tb.stopped() {
			return tb
		}
	}
	return nil
}

func newTab(t target) *tab {
	nextTabID++
	tb := &tab{id: nextTabID, target: t, wantedDisplay: noPreference}
	tabs = append(tabs, tb)
	if len(tabs) == 1 {
		layout() // the strip appears
	}
	pInvalidateRect.Call(hTabs, 0, 0)
	return tb
}

// sendTo queues a message for one connection. Like queue, it drops the
// message if the connection is stalled, so the UI never waits.
func sendTo(l *link, t byte, payload []byte) {
	select {
	case l.out <- [2][]byte{{t}, payload}:
	default:
	}
}

// sendSettings tells a server its connection's settings: in full for the
// connection in front, pared down for the ones behind it.
func sendSettings(l *link) {
	if tb := tabOfLink(l.id); tb != nil {
		sendTo(l, proto.MsgSettings, settingsPayload(tb.opts, cur.Load() == l))
	}
}

func setTabStatus(tb *tab, s string) {
	tb.status = s
	if tb == active {
		setStatus(s)
	}
}

// activate brings a tab to the front (nil: there are none left). byPerson
// says the person switched to it, so the pane's boxes should follow.
func activate(tb *tab, byPerson bool) {
	if tb == active {
		return
	}
	old := active
	releaseKeys() // nothing may stay held down on the connection being left
	clear(pressed)
	active = tb
	var l *link
	if tb != nil {
		l = tb.l
	}
	cur.Store(l)
	framesShown.Store(0)
	measuredFPS = -1
	if old != nil && old.l != nil {
		sendSettings(old.l) // down to the background rate
		if l == nil && old.l.peer != nil {
			old.l.peer.Deactivate()
		}
	}
	if l != nil {
		sendSettings(l)
		if l.peer != nil {
			// Coming from another tab, what was copied while that one was
			// in front should be there to paste on this one.
			l.peer.Activate(old != nil)
		}
	}
	if tb == nil {
		// Back to choosing a connection: the pane shows the one named in
		// the boxes.
		wantedDisplay, wantedRes = noPreference, resolution{}
		if i := findComputer(cleanName(getText(hName))); i >= 0 {
			wantedDisplay, wantedRes = computers[i].display, computers[i].res
			if computers[i].hasOpts {
				showOptions(computers[i].opts)
			}
		}
	} else {
		showOptions(tb.opts) // the pane's settings are this connection's
		if byPerson {
			showInPane(tb)
		}
	}
	refreshPane()
}

// showInPane fills the connection boxes with a tab's connection.
func showInPane(tb *tab) {
	setText(hName, tb.target.name)
	setText(hHost, tb.target.host)
	setText(hPort, tb.target.port)
	setText(hPass, tb.target.password)
	setChecked(hRemember, tb.remember)
	saved := uintptr(0)
	if findComputer(tb.target.name) >= 0 {
		pickedName = tb.target.name
		saved = 1
	}
	pEnableWindow.Call(hForget, saved)
}

// refreshPane brings everything that depends on the tab in front up to
// date: the lists, the buttons, the title, the status text, the picture.
func refreshPane() {
	connected := uintptr(0)
	if cur.Load() != nil {
		connected = 1
	}
	pEnableWindow.Call(hSend, connected)
	pEnableWindow.Call(hFullscreen, connected)
	showTakeControl()
	showDisplays()
	showResolutions()
	refreshConnectButton()
	if active != nil {
		setStatus(active.status)
	}
	refreshUpdate() // also the version line and the layout
	refreshTitle()
	pInvalidateRect.Call(hView, 0, 0)
	pInvalidateRect.Call(hTabs, 0, 0)
}

// refreshTitle names the connection in front in the window's title.
func refreshTitle() {
	title := "Deskbridge"
	if tb := active; tb != nil {
		title += " - " + tb.target.name
		if l := tb.l; l != nil {
			l.fbMu.RLock()
			if l.fbW > 0 {
				title += fmt.Sprintf(" (%d x %d)", l.fbW, l.fbH)
			}
			l.fbMu.RUnlock()
		}
	}
	setTitle(title)
}

// refreshConnectButton labels the pane's main button for the connection
// named in the boxes: Disconnect if it is the tab in front, and so on.
func refreshConnectButton() {
	text, enabled := "Connect", uintptr(1)
	if tb := tabNamed(typedTarget().name); tb != nil && tb == active {
		switch {
		case tb.l != nil:
			text = "Disconnect"
		case tb.reconnecting:
			text = "Stop trying"
		case tb.connecting:
			text, enabled = "Connecting...", 0
		}
	}
	setText(hConnect, text)
	pEnableWindow.Call(hConnect, enabled)
}

// connectClicked runs for the pane's main button, and for Enter anywhere
// in the pane. Only a deliberate press of the button itself disconnects.
func connectClicked() {
	tb := tabNamed(typedTarget().name)
	focus, _, _ := pGetFocus.Call()
	switch {
	case tb != nil && tb == active && (tb.l != nil || tb.reconnecting):
		if focus == hConnect {
			closeTab(tb) // Disconnect, or Stop trying
		}
	case tb != nil && tb == active && tb.connecting:
	default:
		startConnect()
	}
}

// closeTab ends a tab's connection, or its attempts to connect, and
// removes the tab.
func closeTab(tb *tab) {
	pKillTimer.Call(hMain, timerReconnectBase+tb.id)
	wasActive := tb == active
	if wasActive {
		releaseKeys()
		clear(pressed)
		cur.Store(nil)
	}
	if l := tb.l; l != nil {
		tb.l = nil
		l.close() // its receiver's report will find no tab and be ignored
	}
	index := 0
	for i, x := range tabs {
		if x == tb {
			index = i
			tabs = append(tabs[:i], tabs[i+1:]...)
			break
		}
	}
	if !wasActive {
		pInvalidateRect.Call(hTabs, 0, 0)
		refreshUpdate()
		return
	}
	if len(tabs) == 0 {
		if fullscreen {
			toggleFullscreen()
		}
		activate(nil, false)
		setStatus("Disconnected.")
		openPane() // so Connect is within reach
		return
	}
	next := tabs[min(index, len(tabs)-1)]
	activate(next, true)
	if next.l != nil {
		pSetFocus.Call(hView)
	}
}

// stepTab switches to the next or previous tab, wrapping round.
func stepTab(forward bool) {
	if len(tabs) < 2 || active == nil {
		return
	}
	for i, tb := range tabs {
		if tb == active {
			step := len(tabs) - 1
			if forward {
				step = 1
			}
			activate(tabs[(i+step)%len(tabs)], true)
			if cur.Load() != nil {
				pSetFocus.Call(hView)
			}
			return
		}
	}
}

// ---------- connecting ----------

// startConnect connects to whatever is typed in the pane, in a tab of its
// own, or switches to the tab that connection already has.
func startConnect() {
	t := typedTarget()
	if t.host == "" || t.password == "" {
		setStatus("Enter the computer's name or IP address and the password first.")
		return
	}
	if tb := tabFor(t); tb != nil {
		if !tb.stopped() {
			sameName := strings.EqualFold(tb.target.name, t.name)
			activate(tb, !sameName)
			if !sameName {
				setStatus("That PC is already open in the tab \"" + tb.target.name + "\". Close that tab first to connect to it with other settings.")
			}
			refreshConnectButton()
			if tb.l != nil && sameName {
				pSetFocus.Call(hView)
			}
			return
		}
		// A stopped tab of this name: try again with what is typed now.
		tb.target, tb.remember = t, checked(hRemember)
		if tb == active {
			tb.opts = readOptions()
		}
		activate(tb, false)
		connectTo(tb, false)
		return
	}
	// A failed first attempt in front is being corrected (a mistyped
	// address, say): the new attempt takes its tab over.
	reuse := active != nil && active.stopped() && !active.everConnected
	if !reuse && len(tabs) >= maxTabs {
		setStatus("There are " + strconv.Itoa(maxTabs) + " connections open, which is as many as Deskbridge keeps at once. Close a tab first.")
		return
	}
	// The display and resolution this connection starts with: its saved
	// ones; or, when no tab is open, what the resolution list shows.
	shown := resolution{}
	switch {
	case active == nil:
		shown = wantedRes
	case reuse:
		shown = active.wantedRes
	}
	// Its settings: what the pane shows, except that with another tab in
	// front the pane shows that tab's, so a saved connection then starts
	// with its own.
	opts := readOptions()
	if active != nil && !reuse {
		if i := findComputer(t.name); i >= 0 && computers[i].hasOpts {
			opts = computers[i].opts
		}
	}
	var tb *tab
	if reuse {
		tb = active
		tb.target = t
	} else {
		tb = newTab(t)
	}
	tb.opts = opts
	tb.remember = checked(hRemember)
	tb.wantedDisplay, tb.wantedRes = noPreference, shown
	if i := findComputer(t.name); i >= 0 {
		tb.wantedDisplay, tb.wantedRes = computers[i].display, computers[i].res
	} else if i := entryFor(t); i >= 0 {
		tb.wantedDisplay = computers[i].display // being renamed: still that connection
		if active != nil && !reuse {
			tb.wantedRes = computers[i].res
		}
	}
	activate(tb, false)
	connectTo(tb, false)
}

// connectTo starts a connection attempt for a tab in the background. auto
// marks an attempt made by reconnecting rather than by the person.
func connectTo(tb *tab, auto bool) {
	if tb.connecting || tb.l != nil {
		return
	}
	tb.connecting = true
	timeout := 8 * time.Second
	if auto {
		timeout = 4 * time.Second
	} else {
		tb.viewNote = "Connecting to " + tb.target.name + "..."
		setTabStatus(tb, "Connecting to "+tb.target.label()+"...")
	}
	nextLinkID++
	id, tabID, t := nextLinkID, tb.id, tb.target
	go func() {
		res := &connectResult{tab: tabID, target: t, auto: auto}
		addr := net.JoinHostPort(t.host, t.port)
		nc, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			res.err = "Could not reach " + addr + ".\r\n\r\nCheck that Deskbridge is running on that PC and that both PCs are on the same network."
		} else {
			if tc, ok := nc.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			tc, err := proto.ClientHandshake(nc, proto.DeriveKey(t.password))
			if errors.Is(err, proto.ErrAuth) {
				nc.Close()
				res.err, res.badPassword = "Wrong password.", true
			} else if err != nil {
				nc.Close()
				res.err = "Connection failed: " + err.Error()
			} else {
				res.l = newLink(id, proto.NewConn(tc), t.name)
			}
		}
		connectResults <- res
		pPostMessage.Call(hMain, wmConnectDone, 0, 0)
	}()
	if tb == active {
		refreshConnectButton()
		refreshTitle()
		pInvalidateRect.Call(hView, 0, 0)
	}
	pInvalidateRect.Call(hTabs, 0, 0)
}

// connectAtLaunch connects to the saved computer when the client starts,
// and keeps trying if it is not answering yet (still starting up, say).
func connectAtLaunch() {
	t := typedTarget()
	tb := newTab(t)
	tb.remember = checked(hRemember)
	tb.opts = readOptions() // loaded from the saved connection at start
	if i := findComputer(t.name); i >= 0 {
		tb.wantedDisplay, tb.wantedRes = computers[i].display, computers[i].res
	}
	tb.reconnecting = true
	tb.viewNote = "Connecting to " + t.name + "..."
	tb.status = "Connecting to " + t.label() + "..."
	activate(tb, false)
	connectTo(tb, true)
}

// onConnectDone takes in the attempts that have finished.
func onConnectDone() {
	for {
		select {
		case res := <-connectResults:
			connectFinished(res)
		default:
			return
		}
	}
}

func connectFinished(res *connectResult) {
	tb := tabByID(res.tab)
	if tb == nil || tb.l != nil || (res.auto && !tb.reconnecting) {
		// The tab was closed, or its reconnecting stopped, while this
		// attempt was under way.
		if res.l != nil {
			res.l.close()
		}
		if tb != nil {
			tb.connecting = false
		}
		return
	}
	tb.connecting = false
	if res.l == nil {
		switch {
		case tb.reconnecting && res.badPassword:
			stopReconnect(tb, "Reconnecting stopped: the password was not accepted. It may have been changed on that PC.\r\n\r\nEnter the new one and click Connect.", "The password was not accepted")
		case tb.reconnecting:
			scheduleReconnect(tb)
		default:
			tb.viewNote = "Could not connect to " + tb.target.name
			if res.badPassword {
				tb.viewNote = "Wrong password for " + tb.target.name
			}
			setTabStatus(tb, res.err)
			if tb == active {
				openPane() // so the reason is visible
				refreshConnectButton()
				pInvalidateRect.Call(hView, 0, 0)
			}
			pInvalidateRect.Call(hTabs, 0, 0)
		}
		return
	}
	l := res.l
	backToFullscreen := tb == active && tb.reconnecting && tb.restoreFullscreen
	pKillTimer.Call(hMain, timerReconnectBase+tb.id)
	tb.reconnecting, tb.attempt, tb.viewNote, tb.restoreFullscreen = false, 0, "", false
	tb.everConnected = true
	tb.displayRequested, tb.resRequested = false, false
	tb.watching = false
	tb.l = l
	if tb == active {
		cur.Store(l)
		if l.peer != nil {
			l.peer.Activate(false)
		}
	}
	go sender(l)
	go receiver(l)
	go heartbeat(l)
	sendSettings(l)
	tb.status = "Connected to " + tb.target.name + ".\r\n\r\nClick the remote screen to type on the remote PC. Click in this pane to get your own keyboard back." + videoNote()
	if !res.auto {
		rememberComputer(tb)
	}
	saveSettings()
	if tb != active {
		pInvalidateRect.Call(hTabs, 0, 0)
		refreshUpdate()
		return
	}
	refreshPane()
	if backToFullscreen {
		toggleFullscreen()
	}
	pSetFocus.Call(hView)
}

// onDisconnected runs when a live connection has ended by itself: the
// network, the remote PC restarting, or another PC taking it over. (A tab
// the person closes is detached from its connection first, so that ending
// is not seen here.)
func onDisconnected(id uintptr) {
	tb := tabOfLink(id)
	if tb == nil {
		return
	}
	l := tb.l
	tb.l = nil
	l.close()
	wasFullscreen := false
	if tb == active {
		cur.Store(nil)
		clear(pressed)
		wasFullscreen = fullscreen
		if fullscreen {
			toggleFullscreen()
		}
	}
	if l.replaced.Load() {
		// Reconnecting would only start a tug of war.
		tb.viewNote = tb.target.name + " dropped this connection to make room for another PC"
		tb.status = "Disconnected: " + tb.target.name + " dropped this connection to make room for another PC.\r\n\r\nClick Connect to connect again."
		if tb == active {
			openPane()
		}
	} else {
		// A network blip, the remote PC restarting, this PC waking from
		// sleep: keep trying, without getting in the way.
		tb.reconnecting, tb.attempt, tb.restoreFullscreen = true, 0, wasFullscreen
		scheduleReconnect(tb)
	}
	if tb == active {
		refreshPane()
	} else {
		pInvalidateRect.Call(hTabs, 0, 0)
		refreshUpdate()
	}
}

// ---------- reconnecting ----------

// scheduleReconnect arranges a tab's next attempt: after 1, 2, 4 and then
// every 8 seconds.
func scheduleReconnect(tb *tab) {
	tb.attempt++
	delay := 1000 << min(tb.attempt-1, 3)
	s := ""
	if updateNote != "" && time.Since(updateNoteAt) < 3*time.Minute {
		s = updateNote + "\r\n\r\n" // expected: the remote PC is restarting for its update
	}
	if tb.everConnected {
		tb.viewNote = "Connection lost. Reconnecting..."
		s += "The connection to " + tb.target.name + " was lost. Reconnecting (attempt " + strconv.Itoa(tb.attempt) + ")..."
	} else {
		tb.viewNote = "Trying to reach " + tb.target.name + "..."
		s += tb.target.label() + " is not answering yet. Trying again (attempt " + strconv.Itoa(tb.attempt) + ")..."
	}
	setTabStatus(tb, s+"\r\n\r\nClick \"Stop trying\", or close the tab, to give up.")
	pSetTimer.Call(hMain, timerReconnectBase+tb.id, uintptr(delay), 0)
	if tb == active {
		refreshConnectButton()
		pInvalidateRect.Call(hView, 0, 0)
	}
	pInvalidateRect.Call(hTabs, 0, 0)
}

// stopReconnect gives up on a tab because trying again cannot help (a
// changed password). The tab stays, so the cause can be put right.
func stopReconnect(tb *tab, why, note string) {
	tb.reconnecting, tb.attempt, tb.restoreFullscreen = false, 0, false
	tb.viewNote = note
	pKillTimer.Call(hMain, timerReconnectBase+tb.id)
	setTabStatus(tb, why)
	if tb == active {
		openPane() // so Connect is within reach
		refreshConnectButton()
		pInvalidateRect.Call(hView, 0, 0)
	}
	pInvalidateRect.Call(hTabs, 0, 0)
}

// reconnectTimer fires when it is time for a tab's next attempt.
func reconnectTimer(timer uintptr) {
	pKillTimer.Call(hMain, timer)
	if tb := tabByID(timer - timerReconnectBase); tb != nil && tb.reconnecting {
		connectTo(tb, true)
	}
}

// ---------- the strip of tabs ----------

// tabWidth is how wide each tab is drawn, in pixels.
func tabWidth() int {
	var rc rect
	pGetClientRect.Call(hTabs, uintptr(unsafe.Pointer(&rc)))
	if len(tabs) == 0 {
		return sc(190)
	}
	return max(min(sc(190), int(rc.right)/len(tabs)), sc(48))
}

const tabCloseWidth = 24 // the x at a tab's right-hand end, at 96 dpi

func tabsProc(h, message, wParam, lParam uintptr) uintptr {
	at := func() (*tab, bool) {
		x := int(int16(lParam))
		w := tabWidth()
		if i := x / w; x >= 0 && i < len(tabs) {
			return tabs[i], x >= (i+1)*w-sc(tabCloseWidth)
		}
		return nil, false
	}
	switch message {
	case wmPaint:
		paintTabs(h)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmLButtonDown:
		if tb, onClose := at(); tb != nil {
			if onClose {
				closeTab(tb)
			} else {
				activate(tb, true)
				if cur.Load() != nil {
					pSetFocus.Call(hView)
				}
			}
		}
		return 0
	case wmMButtonUp:
		if tb, _ := at(); tb != nil {
			closeTab(tb)
		}
		return 0
	}
	r, _, _ := pDefWindowProc.Call(h, message, wParam, lParam)
	return r
}

func paintTabs(h uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(h, uintptr(unsafe.Pointer(&ps)))
	defer pEndPaint.Call(h, uintptr(unsafe.Pointer(&ps)))
	var rc rect
	pGetClientRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	// System colours: 5 window, 13 highlight, 15 button face, 16 button
	// shadow, 17 grey text, 18 button text.
	brush := func(colour uintptr) uintptr { b, _, _ := pGetSysColorBrush.Call(colour); return b }
	colour := func(c uintptr) uintptr { v, _, _ := pGetSysColor.Call(c); return v }
	fill := func(l, t, r, b int, c uintptr) {
		area := rect{int32(l), int32(t), int32(r), int32(b)}
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&area)), brush(c))
	}
	text := func(s string, l, t, r, b int, c uintptr, flags uintptr) {
		area := rect{int32(l), int32(t), int32(r), int32(b)}
		pSetTextColor.Call(hdc, colour(c))
		pDrawText.Call(hdc, uintptr(unsafe.Pointer(u16ptr(s))), ^uintptr(0), uintptr(unsafe.Pointer(&area)), flags)
	}
	const single, vcentre, centre, noPrefix, ellipsis = 0x20, 0x4, 0x1, 0x800, 0x8000
	height := int(rc.bottom)
	fill(0, 0, int(rc.right), height, 15)
	fill(0, height-1, int(rc.right), height, 16)
	old, _, _ := pSelectObject.Call(hdc, hFont)
	defer pSelectObject.Call(hdc, old)
	pSetBkMode.Call(hdc, 1) // transparent
	w := tabWidth()
	for i, tb := range tabs {
		x := i * w
		label, ink := tb.target.name, uintptr(18)
		switch {
		case tb.l != nil:
			if !tb.l.inControl.Load() {
				label += "  (watching)"
			}
		case tb.stopped():
			label, ink = label+"  (not connected)", 17
		default:
			label, ink = label+"  (connecting...)", 17
		}
		if tb == active {
			fill(x, 0, x+w, height, 5)
			fill(x, 0, x+w, sc(2), 13)
		} else {
			fill(x+w-1, sc(6), x+w, height-sc(6), 16)
		}
		text(label, x+sc(10), 0, x+w-sc(tabCloseWidth), height, ink, single|vcentre|noPrefix|ellipsis)
		text("×", x+w-sc(tabCloseWidth), 0, x+w-sc(2), height, 17, single|vcentre|centre|noPrefix)
	}
}

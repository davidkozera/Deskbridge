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

	// From the settings file: the tabs that were open when the client was
	// last closed, and the one that was in front. launchTabsKnown is false
	// for a file from before these were kept.
	launchTabs      []string
	launchFront     string
	launchTabsKnown bool
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
	saveSettings() // it is not to be opened again next time
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

// reopenTabs opens the tabs that were open when the client was last
// closed: each saved connection among them that has a remembered password.
// Like connectAtLaunch, each keeps trying until its PC answers.
func reopenTabs() {
	var opened []*tab
	var front *tab
	for _, name := range launchTabs {
		i := findComputer(name)
		if i < 0 || tabNamed(name) != nil || len(tabs) >= maxTabs {
			continue
		}
		c := computers[i]
		pw := dpapi(pCryptUnprotectData, c.password)
		if pw == nil {
			continue // no remembered password: it cannot be opened unasked
		}
		t := target{name: c.name, host: c.host, port: c.port, password: string(pw)}
		tb := newTab(t)
		tb.remember = true
		tb.opts = readOptions()
		if c.hasOpts {
			tb.opts = c.opts
		}
		tb.wantedDisplay, tb.wantedRes = c.display, c.res
		tb.reconnecting = true
		tb.viewNote = "Connecting to " + t.name + "..."
		tb.status = "Connecting to " + t.label() + "..."
		opened = append(opened, tb)
		if front == nil || strings.EqualFold(name, launchFront) {
			front = tb
		}
	}
	if front == nil {
		return
	}
	activate(front, true)
	for _, tb := range opened {
		connectTo(tb, true)
	}
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
//
// Along the top of the remote screen: a tab for each connection, then
// "Send" (the same menu as the pane's "Send to remote PC") and "Full
// screen". In full screen the strip is put away, and comes back as a
// toolbar, centred at the top, when the pointer rests at the top edge of
// the screen above it; it goes again once the pointer has left it.

const (
	tabCloseWidth  = 24 // the x at a tab's right-hand end, at 96 dpi
	stripSendWidth = 74
	stripFullWidth = 124

	timerBarShow = 5 // the pointer has rested at the top edge: show the toolbar
	timerBarHide = 6 // while the toolbar is on show: has the pointer left it?
)

var (
	// Touched only on the UI thread.
	barShown  bool // in full screen: the toolbar is on show
	barArmed  bool // the pointer is at the top edge; the toolbar is about to appear
	barMenu   bool // its Send menu is open
	barMisses int  // how many checks in a row found the pointer away from it
)

// stripParts is where things are along the strip, in pixels.
type stripParts struct {
	width, tabW  int
	sendX, fullX int // where "Send" and "Full screen" start
}

func stripGeometry() stripParts {
	var rc rect
	pGetClientRect.Call(hTabs, uintptr(unsafe.Pointer(&rc)))
	g := stripParts{width: int(rc.right), tabW: sc(190)}
	g.fullX = max(g.width-sc(stripFullWidth), 0)
	g.sendX = max(g.fullX-sc(stripSendWidth), 0)
	if n := len(tabs); n > 0 {
		g.tabW = max(min(sc(190), g.sendX/n), sc(48))
	}
	return g
}

// barSpan is where the full-screen toolbar goes along a screen cw pixels
// wide: only as wide as it needs to be, and centred, so that the corners
// of the remote screen stay reachable.
func barSpan(cw int) (x, w int) {
	w = min(max(len(tabs)*sc(150)+sc(stripSendWidth+stripFullWidth), sc(420)), cw*8/10)
	return (cw - w) / 2, w
}

// showBar shows or hides the full-screen toolbar.
func showBar(on bool) {
	pKillTimer.Call(hMain, timerBarShow)
	barArmed = false
	if barShown == on {
		return
	}
	barShown, barMisses = on, 0
	if on {
		layout() // sized for the tabs there are now
		const hwndTop, noMove, noSize, noActivate = 0, 0x2, 0x1, 0x10
		pSetWindowPos.Call(hTabs, hwndTop, 0, 0, 0, 0, noMove|noSize|noActivate)
		pInvalidateRect.Call(hTabs, 0, 0)
		pSetTimer.Call(hMain, timerBarHide, 250, 0)
		return
	}
	pKillTimer.Call(hMain, timerBarHide)
	if fullscreen {
		pShowWindow.Call(hTabs, 0)
	}
}

// atTopEdge reports whether the pointer, at (cx, cy) in the view, is on
// the strip of screen edge above where the toolbar appears.
func atTopEdge(cx, cy int) bool {
	if cy > sc(1) {
		return false
	}
	var rc rect
	pGetClientRect.Call(hView, uintptr(unsafe.Pointer(&rc)))
	x, w := barSpan(int(rc.right))
	return cx >= x && cx < x+w
}

// barPointer runs as the pointer moves over the remote screen in full
// screen. Resting at the top edge for a moment brings the toolbar out; a
// pointer only passing through does not.
func barPointer(cx, cy int) {
	if barShown {
		return
	}
	switch edge := atTopEdge(cx, cy); {
	case edge && !barArmed:
		barArmed = true
		pSetTimer.Call(hMain, timerBarShow, 300, 0)
	case !edge && barArmed:
		barArmed = false
		pKillTimer.Call(hMain, timerBarShow)
	}
}

// barTimer handles the toolbar's two timers.
func barTimer(timer uintptr) {
	var pt [2]int32
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	if timer == timerBarShow {
		pKillTimer.Call(hMain, timerBarShow)
		barArmed = false
		at := pt
		pScreenToClient.Call(hView, uintptr(unsafe.Pointer(&at)))
		if fullscreen && atTopEdge(int(at[0]), int(at[1])) {
			showBar(true)
		}
		return
	}
	if !fullscreen || !barShown {
		pKillTimer.Call(hMain, timerBarHide)
		return
	}
	if barMenu {
		return
	}
	var rc rect
	pGetWindowRect.Call(hTabs, uintptr(unsafe.Pointer(&rc)))
	if pt[0] >= rc.left && pt[0] < rc.right && pt[1] >= rc.top && pt[1] < rc.bottom+int32(sc(12)) {
		barMisses = 0
	} else if barMisses++; barMisses >= 2 {
		showBar(false)
	}
}

func tabsProc(h, message, wParam, lParam uintptr) uintptr {
	x := int(int16(lParam))
	switch message {
	case wmPaint:
		paintTabs(h)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmLButtonDown:
		g := stripGeometry()
		switch {
		case x >= g.fullX:
			if cur.Load() != nil {
				pPostMessage.Call(hMain, wmFullscreen, 0, 0)
			}
		case x >= g.sendX:
			if cur.Load() != nil {
				var rc rect
				pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
				barMenu = true
				sendMenuAt(rc.left+int32(g.sendX), rc.bottom)
				barMenu = false
			}
		default:
			if i := x / g.tabW; x >= 0 && i < len(tabs) {
				if tb := tabs[i]; x >= (i+1)*g.tabW-sc(tabCloseWidth) {
					closeTab(tb)
				} else {
					activate(tb, true)
					if cur.Load() != nil {
						pSetFocus.Call(hView)
					}
				}
			}
		}
		return 0
	case wmMButtonUp:
		g := stripGeometry()
		if i := x / g.tabW; x >= 0 && x < g.sendX && i < len(tabs) {
			closeTab(tabs[i])
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
	g := stripGeometry()
	fill(0, 0, g.width, height, 15)
	old, _, _ := pSelectObject.Call(hdc, hFont)
	defer pSelectObject.Call(hdc, old)
	pSetBkMode.Call(hdc, 1) // transparent
	w := g.tabW
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
		text("\u00D7", x+w-sc(tabCloseWidth), 0, x+w-sc(2), height, 17, single|vcentre|centre|noPrefix)
	}
	// "Send" and "Full screen", at the right-hand end (over any tabs that
	// did not fit).
	ink := uintptr(18)
	if cur.Load() == nil {
		ink = 17
	}
	fill(g.sendX, 0, g.width, height, 15)
	fill(g.sendX, sc(6), g.sendX+1, height-sc(6), 16)
	fill(g.fullX, sc(6), g.fullX+1, height-sc(6), 16)
	text("Send  \u25BE", g.sendX, 0, g.fullX, height, ink, single|vcentre|centre|noPrefix)
	label := "Full screen"
	if fullscreen {
		label = "Exit full screen"
	}
	text(label, g.fullX, 0, g.width, height, ink, single|vcentre|centre|noPrefix)
	// A line under it; and as a toolbar floating over the picture, down
	// its sides too.
	fill(0, height-1, g.width, height, 16)
	if fullscreen {
		fill(0, 0, 1, height, 16)
		fill(g.width-1, 0, g.width, height, 16)
	}
}

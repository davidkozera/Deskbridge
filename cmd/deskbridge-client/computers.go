//go:build windows

package main

// The list of saved connections, and reconnecting by itself when a
// connection is lost.

import (
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

const (
	idName   = 114
	idForget = 115
	idSave   = 117

	wmNewConnection = wmApp + 18 // main: "New connection..." was picked from the list

	timerFrameRate = 1
	timerReconnect = 2

	maxComputers = 50 // how many saved connections are kept
)

// computer is one saved connection. The name is whatever the person likes
// to call it; a connection that was never given one is called by its host.
// The password is kept only if "Remember password" was ticked when it was
// last used, and then encrypted for this Windows account. Each connection
// also remembers which remote display to show and the resolution to set it
// to.
type computer struct {
	name, host, port string
	password         []byte
	res              resolution   // zero: leave the remote display as it is
	display          int          // noPreference, -1 for all, or a display's index
	modes            []resolution // what its display offered when last connected
}

// target is everything needed to connect.
type target struct{ name, host, port, password string }

// label is how a connection is described in messages.
func (t target) label() string {
	if t.name == "" || strings.EqualFold(t.name, t.host) {
		return t.host
	}
	return t.name + " (" + t.host + ")"
}

var (
	pKillTimer = user32.NewProc("KillTimer")
	pSetTimer  = user32.NewProc("SetTimer")

	// Touched only on the UI thread.
	hForget, hSave    uintptr
	computers         []computer // most recently used first
	pickedName        string     // the saved connection the boxes were last filled from
	lastTarget        target     // what the current or last connection used
	haveTarget        bool
	reconnecting      bool
	attempt           int
	everConnected     bool   // a connection has worked since the client started
	userClosed        bool   // the person pressed Disconnect
	restoreFullscreen bool   // go back to full screen once reconnected
	viewNote          string // shown in the empty remote view instead of "Not connected"

	// replaced is set when the server says another client took our place;
	// reconnecting then would only start a tug of war.
	replaced atomic.Bool
)

// findComputer looks a saved connection up by its name.
func findComputer(name string) int {
	for i, c := range computers {
		if name != "" && strings.EqualFold(c.name, name) {
			return i
		}
	}
	return -1
}

// cleanName makes a typed name safe to keep in the settings file.
func cleanName(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '|' || r < ' ' {
			return -1
		}
		return r
	}, s))
}

// typedTarget reads the boxes. A connection without a name is called by
// its computer; and with only the first box filled in, what is typed there
// is the computer (or the name of a saved connection).
func typedTarget() target {
	t := target{name: cleanName(getText(hName)), host: cleanName(getText(hHost)), port: getText(hPort), password: getText(hPass)}
	if t.host == "" {
		t.host = t.name
		if i := findComputer(t.name); i >= 0 {
			t.host = computers[i].host
		}
	}
	if t.name == "" {
		t.name = t.host
	}
	if t.port == "" {
		t.port = proto.DefaultPort
	}
	return t
}

// currentProfile is the saved connection that display and resolution
// choices belong to: the one in use while connected, otherwise the one
// named in the box. -1 if it is not a saved one.
func currentProfile() int {
	if cur.Load() != nil || reconnecting {
		return findComputer(lastTarget.name)
	}
	return findComputer(cleanName(getText(hName)))
}

// loadProfile takes up a saved connection's display and resolution.
func loadProfile(i int) {
	wantedDisplay, wantedRes = computers[i].display, computers[i].res
	showResolutions()
}

// fillComputers puts the saved connections in the drop-down list, leaving
// what is typed in the box alone.
func fillComputers() {
	typed := getText(hName)
	pSendMessage.Call(hName, 0x014B, 0, 0) // CB_RESETCONTENT
	for _, c := range computers {
		pSendMessage.Call(hName, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr(c.name))))
	}
	if len(computers) > 0 {
		// Last in the list: empties the boxes for one that is not saved yet.
		pSendMessage.Call(hName, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr("New connection..."))))
	}
	setText(hName, typed)
	forgettable := uintptr(0)
	if findComputer(cleanName(typed)) >= 0 {
		forgettable = 1
	}
	pEnableWindow.Call(hForget, forgettable)
}

// computerPicked fills in the rest of the connection chosen from the list.
func computerPicked() {
	i, _, _ := pSendMessage.Call(hName, 0x0147, 0, 0) // CB_GETCURSEL
	index := int(int32(i))
	if index == len(computers) && index > 0 {
		// The list puts the entry's own text in the box after telling us,
		// so empty the boxes once it has.
		pPostMessage.Call(hMain, wmNewConnection, 0, 0)
		return
	}
	if index < 0 || index >= len(computers) {
		return
	}
	c := computers[index]
	pickedName = c.name
	setText(hHost, c.host)
	setText(hPort, c.port)
	password := ""
	if pw := dpapi(pCryptUnprotectData, c.password); pw != nil {
		password = string(pw)
	}
	setText(hPass, password)
	setChecked(hRemember, password != "")
	pEnableWindow.Call(hForget, 1)
	if cur.Load() == nil && !reconnecting {
		loadProfile(index)
	}
}

// newConnection empties the boxes, ready for a connection that is not in
// the list yet.
func newConnection() {
	pickedName = ""
	setText(hName, "")
	setText(hHost, "")
	setText(hPass, "")
	setText(hPort, proto.DefaultPort)
	if cur.Load() == nil && !reconnecting {
		wantedDisplay, wantedRes = noPreference, resolution{}
		showResolutions()
	}
	pEnableWindow.Call(hForget, 0)
	pSetFocus.Call(hName)
}

// entryFor finds the saved connection that what is in the boxes belongs
// to, or -1 if it is a new one. Boxes that were filled from a saved
// connection still belong to it after its name or its computer has been
// changed (that is how one is renamed or given a new address), but not
// after both have, and not if the name now typed is another connection's.
func entryFor(t target) int {
	byName := findComputer(t.name)
	if byName >= 0 {
		return byName
	}
	if i := findComputer(pickedName); i >= 0 && strings.EqualFold(computers[i].host, t.host) {
		return i
	}
	return -1
}

// storeConnection writes what is in the boxes to the list: over the saved
// connection they belong to, or as a new one. inUse says this is the
// connection being made or used right now, whose display and resolution
// are the ones in effect. It returns the entry's place in the list.
func storeConnection(t target, inUse bool) int {
	i := entryFor(t)
	c := computer{display: noPreference}
	if i >= 0 {
		c = computers[i]
	}
	renamedFrom := c.name
	c.name, c.host, c.port = t.name, t.host, t.port
	c.password = nil
	if checked(hRemember) {
		c.password = dpapi(pCryptProtectData, []byte(t.password))
	}
	switch {
	case inUse:
		c.res, c.display = wantedRes, wantedDisplay
	case cur.Load() == nil && !reconnecting:
		c.res = wantedRes // what the resolution list shows
	}
	if i >= 0 {
		computers[i] = c
	} else {
		computers = append([]computer{c}, computers...)
		if len(computers) > maxComputers {
			computers = computers[:maxComputers]
		}
		i = 0
	}
	// Renaming the connection in use: everything that goes by its name
	// follows.
	if renamedFrom != "" && strings.EqualFold(lastTarget.name, renamedFrom) && !strings.EqualFold(renamedFrom, t.name) {
		lastTarget.name = t.name
		if l := cur.Load(); l != nil {
			l.name = t.name
			pPostMessage.Call(hMain, wmRemoteSize, l.id, 0) // the title shows the name
		}
	}
	pickedName = t.name
	setText(hName, t.name)
	setText(hHost, t.host)
	return i
}

// rememberComputer records a connection that was just made, at the top of
// the list.
func rememberComputer(t target) {
	i := storeConnection(t, true)
	c := computers[i]
	computers = append(computers[:i], computers[i+1:]...)
	computers = append([]computer{c}, computers...)
	fillComputers()
	saveSettings()
	checkServers() // the list or a password may have changed
}

// saveConnection runs when Save is clicked: it keeps what is in the boxes
// without connecting, which is also how a connection is renamed.
func saveConnection() {
	t := typedTarget()
	if t.host == "" {
		setStatus("Enter the computer's name or IP address first, and a name for the connection if you want one.")
		return
	}
	using := (cur.Load() != nil || reconnecting) && entryFor(t) >= 0 && entryFor(t) == findComputer(lastTarget.name)
	storeConnection(t, using)
	fillComputers()
	saveSettings()
	setStatus("Saved the connection \"" + t.name + "\" (" + t.host + ").")
	checkServers()
}

// forgetComputer removes the connection named in the box from the list,
// along with its saved password.
func forgetComputer() {
	i := findComputer(cleanName(getText(hName)))
	if i < 0 {
		return
	}
	computers = append(computers[:i], computers[i+1:]...)
	pickedName = ""
	setText(hName, "")
	setText(hHost, "")
	setText(hPass, "")
	setText(hPort, proto.DefaultPort)
	if cur.Load() == nil && !reconnecting {
		wantedDisplay, wantedRes = noPreference, resolution{}
		showResolutions()
	}
	fillComputers()
	saveSettings()
	pSetFocus.Call(hName)
}

// computerLines is the saved list in the form kept in the settings file:
// name|host|port|password|resolution|display|resolutions offered.
func computerLines() []string {
	var lines []string
	for _, c := range computers {
		var modes []string
		for _, m := range c.modes {
			modes = append(modes, m.String())
		}
		lines = append(lines, "connection="+strings.Join([]string{c.name, c.host, c.port, hex.EncodeToString(c.password),
			c.res.String(), strconv.Itoa(c.display), strings.Join(modes, ",")}, "|"))
	}
	return lines
}

// parseConnection reads one saved connection.
func parseConnection(v string) {
	parts := strings.Split(v, "|")
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || findComputer(parts[0]) >= 0 {
		return
	}
	c := computer{name: parts[0], host: parts[1], port: parts[2], display: noPreference}
	if n, err := strconv.Atoi(c.port); err != nil || n <= 0 || n > 65535 {
		c.port = proto.DefaultPort
	}
	c.password, _ = hex.DecodeString(parts[3])
	if len(parts) > 4 {
		c.res, _ = parseResolution(parts[4])
	}
	if len(parts) > 5 {
		if n, err := strconv.Atoi(parts[5]); err == nil && n >= -1 && n < 16 {
			c.display = n
		}
	}
	if len(parts) > 6 {
		for _, m := range strings.Split(parts[6], ",") {
			if r, ok := parseResolution(m); ok && len(c.modes) < 60 {
				c.modes = append(c.modes, r)
			}
		}
	}
	computers = append(computers, c)
}

// parseComputer reads a line saved by a version from before connections
// had names (host|port|password): the connection is called by its host.
func parseComputer(v string) {
	if parts := strings.Split(v, "|"); len(parts) == 3 {
		parseConnection(parts[0] + "|" + v)
	}
}

// ---------- connecting ----------

// startConnect connects to whatever is typed in the pane.
func startConnect() {
	t := typedTarget()
	if t.host == "" || t.password == "" {
		setStatus("Enter the computer's name or IP address and the password first.")
		return
	}
	if i := findComputer(t.name); i >= 0 {
		loadProfile(i) // its display and resolution
	} else if j := findComputer(pickedName); j < 0 || !strings.EqualFold(computers[j].host, t.host) {
		// A new connection: the display chosen for another one, which is
		// not on show in the pane, must not follow it here.
		wantedDisplay = noPreference
	}
	connectTo(t, false)
}

// connectTo starts a connection attempt in the background. auto marks an
// attempt made by reconnecting rather than by the person.
func connectTo(t target, auto bool) {
	if connecting || cur.Load() != nil {
		return
	}
	connecting = true
	timeout := 8 * time.Second
	if auto {
		timeout = 4 * time.Second
	} else {
		pEnableWindow.Call(hConnect, 0)
		setStatus("Connecting to " + t.label() + "...")
	}
	nextLinkID++
	id := nextLinkID
	go func() {
		res := &connectResult{target: t, auto: auto}
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
				res.l = &link{id: id, c: proto.NewConn(tc), name: t.name, out: make(chan [2][]byte, 2048), bulk: make(chan [2][]byte, 2), done: make(chan struct{})}
				res.l.heard.Store(time.Now().UnixNano())
			}
		}
		connectRes.Store(res)
		pPostMessage.Call(hMain, wmConnectDone, 0, 0)
	}()
}

// connectAtLaunch connects to the saved computer when the client starts,
// and keeps trying if it is not answering yet (still starting up, say).
func connectAtLaunch() {
	t := typedTarget()
	if i := findComputer(t.name); i >= 0 {
		loadProfile(i)
	}
	lastTarget, haveTarget = t, true
	reconnecting, attempt = true, 0
	viewNote = "Connecting to " + t.name + "..."
	setText(hConnect, "Stop trying")
	setStatus("Connecting to " + t.label() + "...")
	connectTo(t, true)
}

// ---------- reconnecting ----------

// lostUnexpectedly reports whether the connection that just ended should be
// retried: it had been working, the person did not close it, and nobody
// took it over.
func lostUnexpectedly() bool {
	return haveTarget && !userClosed && !replaced.Load()
}

// scheduleReconnect arranges the next attempt: after 1, 2, 4 and then every
// 8 seconds.
func scheduleReconnect() {
	attempt++
	delay := 1000 << min(attempt-1, 3)
	setText(hConnect, "Stop trying")
	if everConnected {
		viewNote = "Connection lost. Reconnecting..."
		setStatus("The connection to " + lastTarget.name + " was lost. Reconnecting (attempt " + strconv.Itoa(attempt) +
			")...\r\n\r\nClick \"Stop trying\" to give up.")
	} else {
		viewNote = "Trying to reach " + lastTarget.name + "..."
		setStatus(lastTarget.label() + " is not answering yet. Trying again (attempt " + strconv.Itoa(attempt) +
			")...\r\n\r\nClick \"Stop trying\" to give up.")
	}
	pSetTimer.Call(hMain, timerReconnect, uintptr(delay), 0)
	pInvalidateRect.Call(hView, 0, 0)
}

// stopReconnect gives up, at the person's request or because trying again
// cannot help (a changed password).
func stopReconnect(why string) {
	reconnecting, attempt, viewNote, restoreFullscreen = false, 0, "", false
	pKillTimer.Call(hMain, timerReconnect)
	setText(hConnect, "Connect")
	setStatus(why)
	openPane() // so Connect is within reach
	pInvalidateRect.Call(hView, 0, 0)
}

// reconnectTimer fires when it is time for the next attempt.
func reconnectTimer() {
	pKillTimer.Call(hMain, timerReconnect)
	if reconnecting {
		connectTo(lastTarget, true)
	}
}

// heartbeat keeps a connection honest. A dead network link can go unnoticed
// for a long time when the screen is idle and nothing is being sent, so the
// client pings every two seconds; if the server has shown it answers pings
// and then nothing at all arrives for eight seconds, the connection is
// declared lost, which starts reconnecting.
func heartbeat(l *link) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-tick.C:
		}
		select {
		case l.out <- [2][]byte{{proto.MsgPing}, nil}:
		default:
		}
		if l.pong.Load() && time.Since(time.Unix(0, l.heard.Load())) > 8*time.Second {
			l.close()
			return
		}
	}
}

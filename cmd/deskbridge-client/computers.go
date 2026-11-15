//go:build windows

package main

// The list of saved connections, and reconnecting by itself when a
// connection is lost.

import (
	"encoding/hex"
	"strconv"
	"strings"
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
	hForget, hSave uintptr
	computers      []computer // most recently used first
	pickedName     string     // the saved connection the boxes were last filled from
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
// choices belong to: that of the tab in front, or with no tab open the one
// named in the box. -1 if it is not a saved one.
func currentProfile() int {
	if active != nil {
		return findComputer(active.target.name)
	}
	return findComputer(cleanName(getText(hName)))
}

// loadProfile shows a saved connection's display and resolution in the
// pane, ready for connecting. With a tab open the lists are about that tab
// instead, and the saved ones are taken up when the connection is made.
func loadProfile(i int) {
	if active != nil {
		return
	}
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
	loadProfile(index)
}

// newConnection empties the boxes, ready for a connection that is not in
// the list yet.
func newConnection() {
	pickedName = ""
	setText(hName, "")
	setText(hHost, "")
	setText(hPass, "")
	setText(hPort, proto.DefaultPort)
	if active == nil {
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

// storeEntry writes a connection to the saved list: over the entry it
// belongs to (see entryFor), or as a new one. tb is the open tab it
// belongs to, if any, whose display and resolution are the ones in effect.
// It returns the entry's place in the list.
func storeEntry(t target, remember bool, tb *tab) int {
	i := entryFor(t)
	c := computer{display: noPreference}
	if i >= 0 {
		c = computers[i]
	}
	renamedFrom := c.name
	c.name, c.host, c.port = t.name, t.host, t.port
	c.password = nil
	if remember {
		c.password = dpapi(pCryptProtectData, []byte(t.password))
	}
	switch {
	case tb != nil:
		c.res, c.display = tb.wantedRes, tb.wantedDisplay
	case active == nil:
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
	// Renaming a connection that is open: its tab and the title follow.
	if renamedFrom != "" && !strings.EqualFold(renamedFrom, t.name) {
		if open := tabNamed(renamedFrom); open != nil {
			open.target.name = t.name
			pInvalidateRect.Call(hTabs, 0, 0)
			refreshTitle()
		}
	}
	return i
}

// rememberComputer records a connection that was just made, at the top of
// the list.
func rememberComputer(tb *tab) {
	// If the boxes still show it, they belong to the entry from now on.
	shown := tb == active && strings.EqualFold(typedTarget().name, tb.target.name)
	i := storeEntry(tb.target, tb.remember, tb)
	c := computers[i]
	computers = append(computers[:i], computers[i+1:]...)
	computers = append([]computer{c}, computers...)
	if shown {
		pickedName = tb.target.name
		setText(hName, tb.target.name)
		setText(hHost, tb.target.host)
	}
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
	// The open tab, if any, that this saved connection belongs to.
	tb := tabNamed(t.name)
	if i := entryFor(t); i >= 0 {
		tb = tabNamed(computers[i].name)
	}
	storeEntry(t, checked(hRemember), tb)
	if tb != nil {
		tb.remember = checked(hRemember)
	}
	pickedName = t.name
	setText(hName, t.name)
	setText(hHost, t.host)
	fillComputers()
	saveSettings()
	refreshConnectButton()
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
	if active == nil {
		wantedDisplay, wantedRes = noPreference, resolution{}
		showResolutions()
	}
	fillComputers()
	saveSettings()
	refreshConnectButton()
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

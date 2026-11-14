//go:build windows

package main

// The list of saved computers, and reconnecting by itself when a connection
// is lost.

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
	idHost   = 114
	idForget = 115

	timerFrameRate = 1
	timerReconnect = 2
)

// computer is one saved entry. The password is kept only if "Remember
// password" was ticked when it was last used, and then encrypted for this
// Windows account, exactly as the single saved password was before.
type computer struct {
	host, port string
	password   []byte
}

// target is everything needed to connect.
type target struct{ host, port, password string }

var (
	pKillTimer = user32.NewProc("KillTimer")
	pSetTimer  = user32.NewProc("SetTimer")

	// Touched only on the UI thread.
	hForget           uintptr
	computers         []computer // most recently used first
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

func findComputer(host string) int {
	for i, c := range computers {
		if strings.EqualFold(c.host, host) {
			return i
		}
	}
	return -1
}

// fillComputers puts the saved computers in the drop-down list, leaving what
// is typed in the box alone.
func fillComputers() {
	typed := getText(hHost)
	pSendMessage.Call(hHost, 0x014B, 0, 0) // CB_RESETCONTENT
	for _, c := range computers {
		pSendMessage.Call(hHost, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr(c.host))))
	}
	setText(hHost, typed)
	forgettable := uintptr(0)
	if findComputer(typed) >= 0 {
		forgettable = 1
	}
	pEnableWindow.Call(hForget, forgettable)
}

// computerPicked fills in the port and password of the entry chosen from
// the list.
func computerPicked() {
	i, _, _ := pSendMessage.Call(hHost, 0x0147, 0, 0) // CB_GETCURSEL
	index := int(int32(i))
	if index < 0 || index >= len(computers) {
		return
	}
	c := computers[index]
	setText(hPort, c.port)
	password := ""
	if pw := dpapi(pCryptUnprotectData, c.password); pw != nil {
		password = string(pw)
	}
	setText(hPass, password)
	setChecked(hRemember, password != "")
	pEnableWindow.Call(hForget, 1)
}

// rememberComputer records a computer that was just connected to, at the
// top of the list.
func rememberComputer(t target) {
	c := computer{host: t.host, port: t.port}
	if checked(hRemember) {
		c.password = dpapi(pCryptProtectData, []byte(t.password))
	}
	if i := findComputer(t.host); i >= 0 {
		computers = append(computers[:i], computers[i+1:]...)
	}
	computers = append([]computer{c}, computers...)
	if len(computers) > 20 {
		computers = computers[:20]
	}
	fillComputers()
	saveSettings()
}

// forgetComputer removes the computer named in the box from the list, along
// with its saved password.
func forgetComputer() {
	i := findComputer(getText(hHost))
	if i < 0 {
		return
	}
	computers = append(computers[:i], computers[i+1:]...)
	setText(hHost, "")
	setText(hPass, "")
	setText(hPort, proto.DefaultPort)
	fillComputers()
	saveSettings()
	pSetFocus.Call(hHost)
}

// computerLines is the saved list in the form kept in the settings file.
func computerLines() []string {
	var lines []string
	for _, c := range computers {
		lines = append(lines, "computer="+c.host+"|"+c.port+"|"+hex.EncodeToString(c.password))
	}
	return lines
}

func parseComputer(v string) {
	parts := strings.Split(v, "|")
	if len(parts) != 3 || parts[0] == "" || findComputer(parts[0]) >= 0 {
		return
	}
	c := computer{host: parts[0], port: parts[1]}
	if n, err := strconv.Atoi(c.port); err != nil || n <= 0 || n > 65535 {
		c.port = proto.DefaultPort
	}
	c.password, _ = hex.DecodeString(parts[2])
	computers = append(computers, c)
}

// ---------- connecting ----------

// startConnect connects to whatever is typed in the pane.
func startConnect() {
	t := target{getText(hHost), getText(hPort), getText(hPass)}
	if t.host == "" || t.password == "" {
		setStatus("Enter the computer name and the password first.")
		return
	}
	if t.port == "" {
		t.port = proto.DefaultPort
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
		setStatus("Connecting to " + t.host + "...")
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
				res.l = &link{id: id, c: proto.NewConn(tc), host: t.host, out: make(chan [2][]byte, 2048), bulk: make(chan [2][]byte, 2), done: make(chan struct{})}
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
	t := target{getText(hHost), getText(hPort), getText(hPass)}
	if t.port == "" {
		t.port = proto.DefaultPort
	}
	lastTarget, haveTarget = t, true
	reconnecting, attempt = true, 0
	viewNote = "Connecting to " + t.host + "..."
	setText(hConnect, "Stop trying")
	setStatus("Connecting to " + t.host + "...")
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
		setStatus("The connection to " + lastTarget.host + " was lost. Reconnecting (attempt " + strconv.Itoa(attempt) +
			")...\r\n\r\nClick \"Stop trying\" to give up.")
	} else {
		viewNote = "Trying to reach " + lastTarget.host + "..."
		setStatus(lastTarget.host + " is not answering yet. Trying again (attempt " + strconv.Itoa(attempt) +
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

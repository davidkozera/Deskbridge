//go:build windows

package main

// Reaching a PC that is not on this network, through a relay.
//
// A relay is a small program both ends connect out to (see internal/relay
// and the INTERNET section of the README). This client is given its relay
// code once, under "More". From then on:
//
//   - each saved PC it connects to is told the relay's address and key, and
//     answers with the ID it goes by there, which is kept with the saved
//     connection;
//   - opening a saved connection tries the PC's own address first, exactly
//     as before, and goes through the relay if that has not answered within
//     a moment.
//
// Either way the session is the same encrypted, password-checked
// connection; the relay only passes it along.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
	"deskbridge/internal/relay"
	"deskbridge/internal/winclip"
)

const (
	wmRelayInfo = wmApp + 23 // main: a server said how it can be reached through the relay; wParam is the connection's id

	// How long a PC's own address has to answer before the relay is tried
	// as well. A PC on this network answers in a few thousandths of a
	// second, so this is rarely waited out when it matters.
	localHeadStart = 1500 * time.Millisecond
)

// The relay this client uses. Touched only on the UI thread; connections
// being made carry a copy.
var relayAddress, relayKey string

// route is how to reach a server: its own address, and its place at the
// relay if it has one.
type route struct {
	host, port                      string
	relayAddress, relayKey, relayID string
}

// routeTo fills in a route for a connection of the given name. Runs on the
// UI thread.
func routeTo(name, host, port string) route {
	r := route{host: host, port: port}
	if i := findComputer(name); i >= 0 && relayAddress != "" && relay.ValidID(computers[i].relayID) {
		r.relayAddress, r.relayKey, r.relayID = relayAddress, relayKey, computers[i].relayID
	}
	return r
}

// routeError is a failure to reach a server by any way open to it.
type routeError struct {
	local error
	far   error // nil if there was no relay to try
}

func (e *routeError) Error() string { return e.local.Error() }

// dial reaches a server: by its own address if that answers, otherwise
// through the relay. relayed reports which.
func (r route) dial(timeout time.Duration) (nc net.Conn, relayed bool, err error) {
	type result struct {
		nc  net.Conn
		err error
	}
	drop := func(ch chan result) {
		go func() {
			if res := <-ch; res.nc != nil {
				res.nc.Close()
			}
		}()
	}
	local := make(chan result, 1)
	go func() {
		nc, err := net.DialTimeout("tcp", net.JoinHostPort(r.host, r.port), timeout)
		local <- result{nc, err}
	}()
	if r.relayID == "" {
		res := <-local
		if res.err != nil {
			return nil, false, &routeError{local: res.err}
		}
		return res.nc, false, nil
	}
	var localErr, farErr error
	select {
	case res := <-local:
		if res.err == nil {
			return res.nc, false, nil
		}
		localErr = res.err
	case <-time.After(localHeadStart):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel() // only bounds the attempt; an open connection outlives it
	far := make(chan result, 1)
	go func() {
		nc, err := relay.Connect(ctx, r.relayAddress, r.relayKey, r.relayID)
		far <- result{nc, err}
	}()
	for localErr == nil || farErr == nil {
		select {
		case res := <-local:
			if res.err == nil {
				drop(far)
				return res.nc, false, nil
			}
			localErr = res.err
		case res := <-far:
			if res.err == nil {
				if localErr == nil {
					drop(local)
				}
				return res.nc, true, nil
			}
			farErr = res.err
		}
	}
	return nil, false, &routeError{localErr, farErr}
}

// describeRouteError says why a server could not be reached, for the pane.
func describeRouteError(err error, host, port string) string {
	addr := net.JoinHostPort(host, port)
	var re *routeError
	if !errors.As(err, &re) || re.far == nil {
		return "Could not reach " + addr + ".\r\n\r\nCheck that Deskbridge is running on that PC and that both PCs are on the same network."
	}
	s := "Could not reach " + addr + " on this network, and "
	switch {
	case errors.Is(re.far, relay.ErrAway):
		s += "that PC is not connected to the internet relay. It may be off, asleep or offline."
	case errors.Is(re.far, relay.ErrKey):
		s += "the internet relay did not accept this client's relay code. Set the relay code again under More."
	default:
		s += "the internet relay could not be reached (" + re.far.Error() + ")."
	}
	return s
}

// relayHost is the relay's name, for messages.
func relayHost() string {
	if u, err := url.Parse(relayAddress); err == nil && u.Host != "" {
		return u.Host
	}
	return relayAddress
}

// Menu identifiers under "More" (see moreMenu).
const (
	relayUseID = 20
	relayOffID = 21
)

// addInternetMenu adds the relay choices to the "More" menu.
func addInternetMenu(menu uintptr) {
	const separator, checked = 0x0800, 0x0008
	pAppendMenu.Call(menu, separator, 0, 0)
	if relayAddress == "" {
		pAppendMenu.Call(menu, 0, relayUseID, uintptr(unsafe.Pointer(u16ptr("Internet access: use the relay code I copied"))))
		return
	}
	pAppendMenu.Call(menu, checked, relayUseID, uintptr(unsafe.Pointer(u16ptr("Internet access: on  (replace with the relay code I copied)"))))
	pAppendMenu.Call(menu, 0, relayOffID, uintptr(unsafe.Pointer(u16ptr("Internet access: turn off"))))
}

func note(text string, flags uintptr) uintptr {
	r, _, _ := pMessageBoxW.Call(hMain, uintptr(unsafe.Pointer(u16ptr(text))), uintptr(unsafe.Pointer(u16ptr("Deskbridge"))), flags)
	return r
}

// useCopiedRelayCode takes the relay code from the clipboard.
func useCopiedRelayCode() {
	address, key, err := relay.ParseCode(winclip.ReadText(hMain))
	if err != nil {
		note("Copy your relay code first, then choose this again.\r\n\r\nA relay code looks like  https://name.example.net#key  and comes from "+
			"setting up the Deskbridge relay (see INTERNET in the README).\r\n\r\nWhat is copied now is not one: "+err.Error()+".", 0x30)
		return
	}
	relayAddress, relayKey = address, key
	saveSettings()
	told := 0
	for _, tb := range tabs {
		if tb.l != nil {
			tb.l.relayTold = false
			if offerRelay(tb) {
				told++
			}
		}
	}
	s := "Internet access is on, through " + relayHost() + ".\r\n\r\nEach saved PC you connect to on its own network is set up to be reachable from anywhere. " +
		"After that, its saved connection works wherever you are."
	if told > 0 {
		s += "\r\n\r\nThe PCs connected now are being set up."
	}
	openPane()
	setStatus(s)
	refreshVersionLine()
}

// turnRelayOff stops this client using the relay, and optionally the PCs
// connected now.
func turnRelayOff() {
	live := 0
	for _, tb := range tabs {
		if tb.l != nil {
			live++
		}
	}
	also := false
	if live > 0 {
		// MB_YESNOCANCEL | MB_ICONQUESTION; 6 is Yes, 7 is No.
		switch note("Also stop the remote PCs connected now from using the relay?\r\n\r\nYes: they can no longer be reached over the internet by any client, "+
			"until one sets them up again.\r\nNo: only this client stops using the relay.", 0x3|0x20) {
		case 6:
			also = true
		case 7:
		default:
			return
		}
	}
	if also {
		for _, tb := range tabs {
			if tb.l != nil {
				sendTo(tb.l, proto.MsgRelaySet, nil)
			}
		}
	}
	relayAddress, relayKey = "", ""
	saveSettings()
	openPane()
	setStatus("Internet access is off in this client. Saved connections are reached on the local network only.")
	refreshVersionLine()
}

// offerRelay tells a connected server which relay to use, once for each
// connection, if this client has one and the connection is a saved one.
// The server answers with its ID there (see relayInfoArrived).
func offerRelay(tb *tab) bool {
	l := tb.l
	if l == nil || l.relayTold || relayAddress == "" || findComputer(tb.target.name) < 0 {
		return false
	}
	l.relayTold = true
	sendTo(l, proto.MsgRelaySet, []byte(relayAddress+"\n"+relayKey))
	return true
}

// relayInfoArrived runs when a server has said how it can be reached
// through a relay. The first time, that prompts this client to tell it
// which relay to use; its answer to that is what gets remembered.
func relayInfoArrived(tb *tab) {
	l := tb.l
	if l == nil {
		return
	}
	info := l.relayInfo.Load()
	if info == nil || relayAddress == "" {
		return
	}
	if offerRelay(tb) {
		return // wait for its answer
	}
	i := findComputer(tb.target.name)
	if i < 0 {
		return
	}
	id, address, _ := strings.Cut(*info, "\n")
	if address != relayAddress || !relay.ValidID(id) {
		id = "" // it is not at this client's relay
	}
	if computers[i].relayID != id {
		computers[i].relayID = id
		saveSettings()
	}
}

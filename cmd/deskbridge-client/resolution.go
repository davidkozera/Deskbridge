//go:build windows

package main

// The "Remote resolution" list: setting the remote display to a resolution
// of the person's choosing, typically one that suits the screen they are
// sitting at. The choice belongs to the saved connection, can be made
// before connecting, and is applied by itself each time that connection is
// made. The remote PC puts its display back a minute after disconnecting.

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"deskbridge/internal/proto"
)

const (
	wmModes = wmApp + 16 // main: the server said which resolutions its display offers

	idResolution = 116
)

type resolution struct{ w, h int }

func (r resolution) String() string {
	if r.w == 0 {
		return ""
	}
	return strconv.Itoa(r.w) + "x" + strconv.Itoa(r.h)
}

func parseResolution(s string) (resolution, bool) {
	a, b, ok := strings.Cut(s, "x")
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || w < 320 || h < 200 || w > 65535 || h > 65535 {
		return resolution{}, false
	}
	return resolution{w, h}, true
}

// modeList is what the server reported about the display being shown.
type modeList struct {
	current resolution
	list    []resolution // empty when all displays are shown together
}

// Offered before the remote PC has said what its display can really do.
var usualResolutions = []resolution{
	{3840, 2160}, {3440, 1440}, {2880, 1800}, {2560, 1600}, {2560, 1440}, {2560, 1080}, {1920, 1200},
	{1920, 1080}, {1680, 1050}, {1600, 900}, {1440, 900}, {1366, 768}, {1280, 800}, {1280, 720},
}

var (
	// Touched only on the UI thread.
	hRes uintptr
	// wantedRes is the resolution for the connection being set up, while
	// no tab is open (zero: leave the remote display as it is). Each tab
	// has its own.
	wantedRes  resolution
	resChoices []resolution // what each entry in the list stands for
)

// parseModes reads a MsgModes payload.
func parseModes(p []byte) *modeList {
	if len(p) < 5 || len(p) < 5+4*int(p[4]) {
		return nil
	}
	u16 := func(at int) int { return int(binary.LittleEndian.Uint16(p[at:])) }
	m := &modeList{current: resolution{u16(0), u16(2)}}
	for i := 0; i < int(p[4]); i++ {
		m.list = append(m.list, resolution{u16(5 + 4*i), u16(7 + 4*i)})
	}
	return m
}

func hasResolution(list []resolution, r resolution) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

// displayPending reports that a tab's remote PC has been asked to show
// another display and has not switched to it yet.
func displayPending(tb *tab) bool {
	d := tb.l.displays.Load()
	if d == nil || tb.wantedDisplay == noPreference {
		return false
	}
	valid := tb.wantedDisplay < len(d.list) && (tb.wantedDisplay >= 0 || len(d.list) > 1)
	return valid && tb.wantedDisplay != d.selected
}

// modesArrived runs when a tab's server has said which resolutions the
// display it is showing offers.
func modesArrived(tb *tab) {
	m := tb.l.modes.Load()
	if m == nil {
		return
	}
	if i := findComputer(tb.target.name); i >= 0 && len(m.list) > 0 && fmt.Sprint(computers[i].modes) != fmt.Sprint(m.list) {
		computers[i].modes = m.list // so the list is right before connecting next time
		saveSettings()
	}
	// Set the display to this connection's resolution, once it is showing
	// the display this connection wants. This applies to tabs behind the
	// one in front too.
	if len(m.list) > 0 && !tb.resRequested && !displayPending(tb) {
		tb.resRequested = true
		if r := tb.wantedRes; r.w != 0 && r != m.current && hasResolution(m.list, r) {
			sendTo(tb.l, proto.MsgSetResolution, resolutionPayload(r))
		}
	}
	if tb == active {
		showResolutions()
	}
}

// showResolutions fills the list for the tab in front: with what the
// remote display offers while connected, and otherwise (or with no tab
// open, for the connection named in the boxes) with what it offered last
// time, or the usual sizes for a connection that has not been made yet.
func showResolutions() {
	connected := cur.Load() != nil
	wantedRes := wantedRes
	var m *modeList
	if active != nil {
		wantedRes = active.wantedRes
		if active.l != nil {
			m = active.l.modes.Load()
		}
	}
	var offered []resolution
	known, usable := false, true
	if connected && m != nil {
		offered, known, usable = m.list, true, len(m.list) > 0
	} else if i := currentProfile(); i >= 0 && len(computers[i].modes) > 0 {
		offered, known = computers[i].modes, true
	} else {
		offered = usualResolutions
	}

	pSendMessage.Call(hRes, 0x014B, 0, 0) // CB_RESETCONTENT
	add := func(s string) { pSendMessage.Call(hRes, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr(s)))) }
	resChoices = []resolution{{}}
	add("Leave as it is")
	for _, r := range offered {
		resChoices = append(resChoices, r)
		add(fmt.Sprintf("%d x %d", r.w, r.h))
	}
	if wantedRes.w != 0 && !hasResolution(offered, wantedRes) {
		resChoices = append(resChoices, wantedRes)
		if known && usable {
			add(fmt.Sprintf("%d x %d  (not offered by this display)", wantedRes.w, wantedRes.h))
		} else {
			add(fmt.Sprintf("%d x %d", wantedRes.w, wantedRes.h))
		}
	}
	for i, r := range resChoices {
		if r == wantedRes {
			pSendMessage.Call(hRes, 0x014E, uintptr(i), 0) // CB_SETCURSEL
		}
	}
	enable := uintptr(1)
	if !usable {
		enable = 0 // all displays are shown together: there is no one display to set
	}
	pEnableWindow.Call(hRes, enable)
}

func resolutionPayload(r resolution) []byte {
	p := binary.LittleEndian.AppendUint16(nil, uint16(r.w))
	return binary.LittleEndian.AppendUint16(p, uint16(r.h))
}

// resolutionPicked runs when the person chooses an entry.
func resolutionPicked() {
	i, _, _ := pSendMessage.Call(hRes, 0x0147, 0, 0) // CB_GETCURSEL
	index := int(int32(i))
	if index < 0 || index >= len(resChoices) {
		return
	}
	r := resChoices[index]
	if active != nil {
		active.wantedRes, active.resRequested = r, true
	} else {
		wantedRes = r
	}
	if p := currentProfile(); p >= 0 {
		computers[p].res = r
	}
	saveSettings()
	if cur.Load() != nil {
		queue(proto.MsgSetResolution, resolutionPayload(r)) // 0 x 0 puts the display back as it was
		pSetFocus.Call(hView)
	}
}

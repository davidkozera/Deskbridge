//go:build windows

package main

// The release channel: which published versions this client looks for.
//
// Mainline is the finished versions. Beta and Alpha are test versions,
// published from branches of their own, with Alpha the least settled. The
// choice is made under "More" and kept with the other settings. It belongs
// to this client: the Update button then moves this PC, and every server it
// is connected to or has saved, to the newest version of that channel.
// Going back to Mainline works the same way, and is the one case where a
// PC is moved to an older version (see update.Allowed).

import (
	"sync/atomic"
	"unsafe"

	"deskbridge/internal/update"
)

var (
	releaseChannel atomic.Int32 // an update.Alpha, Beta or Mainline
	checkNow       = make(chan struct{}, 1)
)

func init() { releaseChannel.Store(update.Mainline) }

// Menu identifiers under "More" (see moreMenu).
const channelMenuBase = 10 // + the channel

// addChannelMenu adds the channel choices to the "More" menu, with a mark
// beside the one in use.
func addChannelMenu(menu uintptr) {
	const separator, checked = 0x0800, 0x0008
	pAppendMenu.Call(menu, separator, 0, 0)
	for _, c := range []struct {
		channel int
		text    string
	}{
		{update.Mainline, "Releases: Mainline"},
		{update.Beta, "Releases: Beta (test versions)"},
		{update.Alpha, "Releases: Alpha (experimental)"},
	} {
		flags := uintptr(0)
		if int(releaseChannel.Load()) == c.channel {
			flags = checked
		}
		pAppendMenu.Call(menu, flags, uintptr(channelMenuBase+c.channel), uintptr(unsafe.Pointer(u16ptr(c.text))))
	}
}

// channelPicked runs when a channel is chosen from the menu.
func channelPicked(channel int) {
	if channel == int(releaseChannel.Load()) {
		return
	}
	if updating {
		setStatus("Wait for the update under way to finish before changing the release channel.")
		return
	}
	if channel != update.Mainline {
		question := update.ChannelName(channel) + " versions are test versions and may not work properly.\r\n\r\n" +
			"After choosing this, the Update button offers to move this PC and its remote PCs to the newest " +
			update.ChannelName(channel) + " version. Nothing changes until you click it.\r\n\r\n" +
			"To go back, choose Releases: Mainline here and click the button again. If a remote PC can no longer be " +
			"reached at all, run the Mainline setup program on it.\r\n\r\nFollow " + update.ChannelName(channel) + " releases?"
		// MB_YESNO | MB_ICONWARNING | MB_DEFBUTTON2; 6 is "Yes".
		if r, _, _ := pMessageBoxW.Call(hMain, uintptr(unsafe.Pointer(u16ptr(question))),
			uintptr(unsafe.Pointer(u16ptr("Deskbridge"))), 0x4|0x30|0x100); r != 6 {
			return
		}
	}
	releaseChannel.Store(int32(channel))
	saveSettings()
	openPane()
	setStatus("Following " + update.ChannelName(channel) + " releases. Looking for the newest version...")
	select { // look now, not at the next routine check
	case checkNow <- struct{}{}:
	default:
	}
	refreshUpdate()
}

//go:build windows

package main

// The Update button.
//
// The client looks for a newer published version when it starts (and a few
// times a day after that). It also learns the connected server's version,
// and asks the other saved servers for theirs (see fleet.go). If this PC or
// any of those servers is behind, an Update button appears in the pane and
// the window title says so.
//
// Which versions count is set by the release channel chosen under "More":
// Mainline, Beta or Alpha (see channel.go). Changing it offers to move this
// PC and its servers to that channel's newest version, which can be an
// older one when leaving Alpha or Beta.
//
// One click then: downloads the setup program and checks its signature;
// sends it to the connected server over the existing connection and to the
// other saved servers over connections of their own, all at once, where it
// is checked again and installed; and finally updates this PC, which needs
// one Windows administrator prompt, after which the client reopens by
// itself.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
	"deskbridge/internal/update"
)

const (
	wmUpdateInfo   = wmApp + 11 // main: newest version or server version learned
	wmUpdateStatus = wmApp + 12 // main: show the text left in updateText
	wmUpdateDone   = wmApp + 13 // main: the update run ended without restarting
	wmRunSetup     = wmApp + 14 // main: start setup to update this PC

	idUpdate = 111
)

var (
	pShellExecute  = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
	pDestroyWindow = user32.NewProc("DestroyWindow")

	latestRelease atomic.Pointer[foundRelease] // newest published version, once known
	updateText    atomic.Pointer[string]
	setupFile     atomic.Pointer[string]

	// Touched only on the UI thread.
	hUpdate       uintptr
	updateOffered bool // something can be moved: the button shows
	updateNewer   bool // and at least one of those moves is to a newer version
	updating      bool
	titleBase     = "Deskbridge"
	updateNote    string // what to keep on screen when the server restarts for its update
	updateNoteAt  time.Time
)

// setTitle sets the window title, adding a hint when an update is waiting
// (the pane with the button is usually hidden).
func setTitle(s string) {
	titleBase = s
	if updateNewer {
		s += "  -  update available: open the left pane"
	}
	setText(hMain, s)
}

// watchForUpdates checks for a newer version now and then.
func watchForUpdates() {
	if _, ok := update.Parse(proto.Version); !ok {
		return // a development build
	}
	asked := false // this check follows a change of channel, so say how it went
	for {
		channel := releaseChannel.Load()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rel, err := update.Latest(ctx, int(channel))
		cancel()
		name := update.ChannelName(int(channel))
		switch {
		case err == nil:
			latestRelease.Store(&foundRelease{rel, channel})
			if asked {
				updateProgress("Following " + name + " releases. The newest version is " + rel.Version + ".")
			}
			pPostMessage.Call(hMain, wmUpdateInfo, 0, 0)
		case asked:
			updateProgress("Following " + name + " releases, but the newest version could not be looked up: " + err.Error() + ".")
		}
		pPostMessage.Call(hMain, wmCheckServers, 0, 0)
		select {
		case <-checkNow: // the channel was changed
			asked = true
		case <-time.After(6 * time.Hour):
			asked = false
		}
	}
}

// foundRelease is a published version and the channel it was looked up for.
type foundRelease struct {
	rel     update.Release
	channel int32
}

// liveServer is a connected server that should be updated, with the name
// of its connection.
type liveServer struct {
	l    *link
	name string
}

// updatePlan works out what, if anything, should be updated: the version to
// move to, whether this PC needs it, which of the connected PCs do, and
// which of the other saved servers do.
//
// The version to move to is the newest one published for the chosen
// channel. This PC's own version stands in when that is not known (or is
// older), so servers can still be brought level with this PC, but only if
// this PC's version belongs to the chosen channel.
func updatePlan() (target string, client bool, live []liveServer, rest []savedServer) {
	channel := releaseChannel.Load()
	if own, ok := update.Parse(proto.Version); ok && own.Channel >= int(channel) {
		target = proto.Version
	}
	if r := latestRelease.Load(); r != nil && r.channel == channel && (target == "" || update.Newer(r.rel.Version, target)) {
		target = r.rel.Version
	}
	if target == "" {
		return "", false, nil, nil
	}
	for _, tb := range tabs {
		if l := tb.l; l != nil {
			if v := l.version.Load(); v != nil && update.Allowed(*v, target) {
				live = append(live, liveServer{l, tb.target.name})
			}
		}
	}
	return target, update.Allowed(proto.Version, target), live, behind(target)
}

// refreshUpdate shows or hides the Update button.
func refreshUpdate() {
	refreshVersionLine() // the server's version may have just arrived or gone
	target, client, live, rest := updatePlan()
	count := len(live) + len(rest)
	if client {
		count++
	}
	// How many of those moves are to a newer version. The rest go back to
	// an older one, which only happens when leaving Alpha or Beta: the
	// button then says "Switch", and the window title stays quiet about it.
	newer := 0
	if client && update.Newer(target, proto.Version) {
		newer++
	}
	for _, s := range live {
		if v := s.l.version.Load(); v != nil && update.Newer(target, *v) {
			newer++
		}
	}
	for _, o := range rest {
		if o.version == "" || update.Newer(target, o.version) {
			newer++
		}
	}
	updateOffered, updateNewer = count > 0, newer > 0
	verb := "Update"
	if newer < count {
		verb = "Switch"
	}
	switch {
	case count == 1 && client:
		setText(hUpdate, verb+" to version "+target)
	case count == 1 && len(live) == 1:
		setText(hUpdate, verb+" the remote PC to "+target)
	case count == 1:
		setText(hUpdate, verb+" 1 saved PC to "+target)
	case count == 2 && client && len(live) == 1:
		setText(hUpdate, verb+" both PCs to "+target)
	case count > 1:
		setText(hUpdate, verb+" "+strconv.Itoa(count)+" PCs to "+target)
	}
	enabled := uintptr(1)
	if updating {
		enabled = 0
	}
	pEnableWindow.Call(hUpdate, enabled)
	layout()
	setTitle(titleBase)
}

func updateProgress(s string) {
	updateText.Store(&s)
	pPostMessage.Call(hMain, wmUpdateStatus, 0, 0)
}

// startUpdate runs when the button is clicked.
func startUpdate() {
	target, client, live, rest := updatePlan()
	if updating || (!client && len(live) == 0 && len(rest) == 0) {
		return
	}
	updating = true
	refreshUpdate()
	openPane()
	go func() {
		done := func(s string) {
			updateText.Store(&s)
			pPostMessage.Call(hMain, wmUpdateDone, 0, 0)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		updateProgress("Downloading version " + target + "...")
		var rel update.Release
		if r := latestRelease.Load(); r != nil && r.rel.Version == target {
			rel = r.rel
		} else {
			var err error
			if rel, err = update.ByVersion(ctx, target); err != nil {
				done("Version " + target + " could not be found: " + err.Error() + ".")
				return
			}
		}
		installer, signature, err := update.Download(ctx, rel)
		if err != nil {
			done("The update could not be downloaded: " + err.Error() + ".")
			return
		}

		// The connected servers and the other saved ones, all at once.
		others := make(chan string, 1)
		go func() {
			if len(rest) == 0 {
				others <- ""
				return
			}
			others <- updateServers(rest, target, signature, installer)
		}()
		if len(live) == 1 {
			updateProgress("Sending version " + target + " to the remote PC...")
		} else if len(live)+len(rest) > 0 {
			updateProgress("Sending version " + target + " to the other PCs...")
		}
		verdicts := make([]string, len(live))
		var wg sync.WaitGroup
		for i, s := range live {
			wg.Add(1)
			go func() {
				defer wg.Done()
				verdicts[i] = pushUpdate(s.l, target, signature, installer)
			}()
		}
		wg.Wait()
		remote := ""
		switch {
		case len(live) == 1:
			remote = "The remote PC " + verdicts[0]
		case len(live) > 1:
			remote = "Connected PCs:"
			for i, s := range live {
				remote += "\r\n" + s.name + ": " + verdicts[i]
			}
		}
		if lines := <-others; lines != "" {
			if remote != "" {
				remote += "\r\n\r\n"
			}
			remote += "Other saved PCs:\r\n" + lines
		}
		if !client {
			done(remote)
			return
		}

		dir := filepath.Join(os.TempDir(), "Deskbridge-update")
		os.RemoveAll(dir)
		path := filepath.Join(dir, update.AssetName(target))
		if err := os.MkdirAll(dir, 0700); err == nil {
			err = os.WriteFile(path, installer, 0700)
		}
		if err != nil {
			done(remote + "\r\n\r\nThe update for this PC could not be saved: " + err.Error())
			return
		}
		if remote != "" {
			updateProgress(remote)
		}
		setupFile.Store(&path)
		pPostMessage.Call(hMain, wmRunSetup, 0, 0)
	}()
}

// pushUpdate sends the setup program to a connected server and waits for
// its verdict, which it returns as the end of a sentence about that PC.
func pushUpdate(l *link, version, signature string, installer []byte) string {
	select { // forget any stale answer
	case <-l.updateResult:
	default:
	}
	send := func(t byte, p []byte) bool {
		select {
		case l.bulk <- [2][]byte{{t}, p}:
			return true
		case <-l.done:
			return false
		}
	}
	ok := send(proto.MsgUpdateBegin, []byte(version+"\n"+signature+"\n"+strconv.Itoa(len(installer))))
	for off := 0; ok && off < len(installer); off += 256 << 10 {
		ok = send(proto.MsgUpdateData, installer[off:min(off+256<<10, len(installer))])
	}
	ok = ok && send(proto.MsgUpdateEnd, nil)
	if !ok {
		return "was not updated: the connection closed."
	}
	select {
	case r := <-l.updateResult:
		if len(r) > 0 && r[0] == 1 {
			return "is installing version " + version + ". Its connection drops for a few seconds while it restarts."
		}
		why := "it gave no reason."
		if len(r) > 1 {
			why = string(r[1:])
		}
		return "was not updated: " + why
	case <-l.done:
		return "was not updated: the connection closed."
	case <-time.After(2 * time.Minute):
		return "did not answer the update request."
	}
}

// updateMessage handles the update-related window messages. It reports
// whether the message was one of them.
func updateMessage(message, wParam uintptr) bool {
	switch message {
	case wmUpdateInfo:
		refreshUpdate()
	case wmCheckServers:
		checkServers()
	case wmUpdateStatus:
		if s := updateText.Swap(nil); s != nil {
			updateNote, updateNoteAt = *s, time.Now()
			setStatus(*s)
		}
	case wmUpdateDone:
		updating = false
		if s := updateText.Swap(nil); s != nil && *s != "" {
			updateNote, updateNoteAt = *s, time.Now()
			setStatus(*s)
		}
		refreshUpdate()
	case wmRunSetup:
		path := setupFile.Swap(nil)
		if path == nil {
			break
		}
		setStatus("Approve the Windows prompt to update this PC. Deskbridge closes and reopens by itself.")
		r, _, _ := pShellExecute.Call(hMain, uintptr(unsafe.Pointer(u16ptr("runas"))), uintptr(unsafe.Pointer(u16ptr(*path))),
			uintptr(unsafe.Pointer(u16ptr("/silent /relaunch"))), 0, 1)
		if r > 32 {
			pDestroyWindow.Call(hMain) // setup replaces this program once it has exited
			break
		}
		updating = false
		setStatus("This PC was not updated: the Windows prompt was declined. Click Update to try again.")
		refreshUpdate()
	default:
		return false
	}
	return true
}

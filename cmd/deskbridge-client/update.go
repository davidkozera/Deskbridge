//go:build windows

package main

// The Update button.
//
// The client looks for a newer published version when it starts (and a few
// times a day after that). It also learns the connected server's version.
// If either this PC or the remote PC is behind, an Update button appears in
// the pane and the window title says so.
//
// One click then: downloads the setup program and checks its signature;
// sends it to the server over the existing connection, where it is checked
// again and installed; and finally updates this PC, which needs one Windows
// administrator prompt, after which the client reopens by itself.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

	latestRelease atomic.Pointer[update.Release] // newest published version, once known
	serverVersion atomic.Pointer[string]         // the connected server's version, if it said
	updateText    atomic.Pointer[string]
	setupFile     atomic.Pointer[string]
	updateResult  = make(chan []byte, 1) // the server's answer to an update

	// Touched only on the UI thread.
	hUpdate       uintptr
	updateOffered bool
	updating      bool
	titleBase     = "Deskbridge"
	updateNote    string // what to keep on screen when the server restarts for its update
	updateNoteAt  time.Time
)

// setTitle sets the window title, adding a hint when an update is waiting
// (the pane with the button is usually hidden).
func setTitle(s string) {
	titleBase = s
	if updateOffered {
		s += "  -  update available: open the left pane"
	}
	setText(hMain, s)
}

// watchForUpdates checks for a newer version now and then.
func watchForUpdates() {
	if _, ok := update.Parse(proto.Version); !ok {
		return // a development build
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rel, err := update.Latest(ctx)
		cancel()
		if err == nil {
			latestRelease.Store(&rel)
			pPostMessage.Call(hMain, wmUpdateInfo, 0, 0)
		}
		time.Sleep(6 * time.Hour)
	}
}

// updatePlan works out what, if anything, should be updated: the version to
// move to, and whether this PC and the remote PC each need it.
func updatePlan() (target string, client, server bool) {
	target = proto.Version
	if r := latestRelease.Load(); r != nil && update.Newer(r.Version, target) {
		target = r.Version
	}
	if v := serverVersion.Load(); v != nil && cur.Load() != nil {
		server = update.Newer(target, *v)
	}
	return target, update.Newer(target, proto.Version), server
}

// refreshUpdate shows or hides the Update button.
func refreshUpdate() {
	refreshVersionLine() // the server's version may have just arrived or gone
	target, client, server := updatePlan()
	updateOffered = client || server
	switch {
	case client && server:
		setText(hUpdate, "Update both PCs to "+target)
	case client:
		setText(hUpdate, "Update to version "+target)
	case server:
		setText(hUpdate, "Update the remote PC to "+target)
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
	target, client, server := updatePlan()
	if updating || (!client && !server) {
		return
	}
	updating = true
	refreshUpdate()
	openPane()
	l := cur.Load()
	go func() {
		done := func(s string) {
			updateText.Store(&s)
			pPostMessage.Call(hMain, wmUpdateDone, 0, 0)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		updateProgress("Downloading version " + target + "...")
		var rel update.Release
		if r := latestRelease.Load(); r != nil && r.Version == target {
			rel = *r
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

		remote := ""
		if server && l != nil && cur.Load() == l {
			updateProgress("Sending version " + target + " to the remote PC...")
			remote = pushUpdate(l, target, signature, installer)
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

// pushUpdate sends the setup program to the server and waits for its
// verdict, which it returns as a sentence for the pane.
func pushUpdate(l *link, version, signature string, installer []byte) string {
	select { // forget any stale answer
	case <-updateResult:
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
		return "The remote PC was not updated: the connection closed."
	}
	select {
	case r := <-updateResult:
		if len(r) > 0 && r[0] == 1 {
			return "The remote PC is installing version " + version + ". Its connection drops for a few seconds while it restarts."
		}
		why := "it gave no reason."
		if len(r) > 1 {
			why = string(r[1:])
		}
		return "The remote PC was not updated: " + why
	case <-l.done:
		return "The remote PC was not updated: the connection closed."
	case <-time.After(2 * time.Minute):
		return "The remote PC did not answer the update request."
	}
}

// updateMessage handles the update-related window messages. It reports
// whether the message was one of them.
func updateMessage(message, wParam uintptr) bool {
	switch message {
	case wmUpdateInfo:
		refreshUpdate()
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

//go:build windows

package main

// Updating the server from the client.
//
// When the person clicks Update in the client, the client sends the new
// setup program over the existing connection. The server never trusts it on
// the client's say-so: it checks the publisher's signature (see
// internal/update) and that the version is one it may move to (a newer one,
// or a more settled release channel's: see update.Allowed), and only then
// runs it. Setup replaces the service and starts it again, so the connection
// drops for a few seconds.

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"deskbridge/internal/proto"
	"deskbridge/internal/update"
)

// installing is set once an update has been verified and setup is about to
// run, so that a second copy arriving at the same moment (over another
// connection) does not trip over the first.
var installing atomic.Bool

type updateReceiver struct {
	active       bool
	version, sig string
	want         int
	buf          []byte
}

func (u *updateReceiver) handle(s *session, t byte, p []byte) {
	refuse := func(why string) {
		u.active, u.buf = false, nil
		log.Print("update refused: ", why)
		s.c.Send(proto.MsgUpdateResult, true, append([]byte{0}, why...))
	}
	switch t {
	case proto.MsgUpdateBegin:
		u.active, u.buf = false, nil
		parts := strings.Split(string(p), "\n")
		if len(parts) != 3 {
			refuse("the request was malformed.")
			return
		}
		size, _ := strconv.Atoi(parts[2])
		switch {
		case !workerMode:
			refuse("it was started by hand rather than installed, so it cannot update itself. Run setup on that PC.")
		case parts[0] == proto.Version:
			refuse("it already runs version " + proto.Version + ".")
		case !update.Allowed(proto.Version, parts[0]):
			refuse("it runs version " + proto.Version + ", which cannot be replaced by version " + parts[0] + ".")
		case size <= 0 || size > update.MaxInstaller:
			refuse("the update is an unexpected size.")
		default:
			u.active, u.version, u.sig, u.want, u.buf = true, parts[0], parts[1], size, make([]byte, 0, size)
		}
	case proto.MsgUpdateData:
		if !u.active {
			return
		}
		if len(u.buf)+len(p) > u.want {
			refuse("more data arrived than announced.")
			return
		}
		u.buf = append(u.buf, p...)
	case proto.MsgUpdateEnd:
		if !u.active {
			return
		}
		installer, version := u.buf, u.version
		if len(installer) != u.want {
			refuse("the update arrived incomplete.")
			return
		}
		if err := update.Verify(version, installer, u.sig); err != nil {
			refuse(err.Error())
			return
		}
		u.active, u.buf = false, nil
		if !installing.CompareAndSwap(false, true) {
			refuse("an update is already being installed.")
			return
		}
		// Keep it in our own folder, which only administrators can write
		// to, so nothing can swap the file between this check and running it.
		exe, _ := os.Executable()
		dir := filepath.Join(filepath.Dir(exe), "update")
		os.RemoveAll(dir)
		path := filepath.Join(dir, update.AssetName(version))
		if err := os.MkdirAll(dir, 0755); err != nil {
			installing.Store(false)
			refuse("the update could not be saved: " + err.Error())
			return
		}
		if err := os.WriteFile(path, installer, 0755); err != nil {
			installing.Store(false)
			refuse("the update could not be saved: " + err.Error())
			return
		}
		log.Printf("update: version %s verified; running setup", version)
		s.c.Send(proto.MsgUpdateResult, true, append([]byte{1}, "Version "+version+" was verified and is being installed."...))
		go func() {
			time.Sleep(500 * time.Millisecond) // let the answer reach the client first
			cmd := exec.Command(path, "/silent")
			cmd.Dir = dir
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000200} // no window, own process group
			if err := cmd.Start(); err != nil {
				log.Print("update: setup could not be started: ", err)
				installing.Store(false)
			}
		}()
	}
}

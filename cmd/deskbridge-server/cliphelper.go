//go:build windows

package main

// The clipboard helper, and the worker's side of talking to it.
//
// worker (SYSTEM)  <-- loopback TCP -->  helper (signed-in person)
//
// The helper owns the clipboard. The worker just passes clipboard messages
// between the helper and whichever client is connected, and tells the helper
// when sharing is on. Only a process that knows a random token, handed to
// the helper on its command line, may connect.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
	"deskbridge/internal/winclip"
)

const msgHelperHello = 60 // payload: the token

var (
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	userenv  = syscall.NewLazyDLL("userenv.dll")

	pWTSQueryUserToken       = wtsapi32.NewProc("WTSQueryUserToken")
	pCreateEnvironmentBlock  = userenv.NewProc("CreateEnvironmentBlock")
	pDestroyEnvironmentBlock = userenv.NewProc("DestroyEnvironmentBlock")
	pProcessIdToSessionId    = kernel32.NewProc("ProcessIdToSessionId")

	helper atomic.Pointer[proto.Conn] // the connected helper, if any
)

// ---------- worker side ----------

func startClipRelay() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Print("clipboard sharing unavailable: ", err)
		return
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c := proto.NewConn(nc)
				nc.SetReadDeadline(time.Now().Add(5 * time.Second))
				t, p, err := c.Read()
				if err != nil || t != msgHelperHello || subtle.ConstantTimeCompare(p, []byte(token)) != 1 {
					nc.Close()
					return
				}
				nc.SetReadDeadline(time.Time{})
				if old := helper.Swap(c); old != nil {
					old.Close()
				}
				log.Print("clipboard helper connected")
				clipRelayResume()
				for {
					t, p, err := c.Read()
					if err != nil {
						helper.CompareAndSwap(c, nil)
						c.Close()
						return
					}
					if !proto.IsClipMsg(t) || t == proto.MsgClipState {
						continue
					}
					// Copy and paste is shared with the client in control.
					if s := control.Load(); s != nil && !s.closed() && s.clipOn.Load() {
						s.c.Send(t, true, p)
					}
				}
			}()
		}
	}()

	// Keep a helper running whenever someone is signed in.
	go func() {
		var proc syscall.Handle
		lastErr := ""
		for {
			if proc != 0 {
				if ev, _ := syscall.WaitForSingleObject(proc, 0); ev == 0 { // exited
					syscall.CloseHandle(proc)
					proc = 0
				}
			}
			if proc == 0 {
				h, err := spawnHelper(port, token)
				if err == nil {
					proc, lastErr = h, ""
				} else if err.Error() != lastErr {
					lastErr = err.Error()
					log.Print("clipboard helper not started (normal while nobody is signed in): ", err)
				}
			}
			time.Sleep(3 * time.Second)
		}
	}()
}

// What the helper can be told to do.
const (
	clipOff = 0 // stop sharing
	clipOn  = 1 // share from now on
)

// clipRelayState tells the helper whether to share.
func clipRelayState(state byte) {
	if h := helper.Load(); h != nil {
		h.Send(proto.MsgClipState, true, []byte{state})
	}
}

// clipRelayResume puts the helper in the state the client in control
// calls for: after the helper (re)starts, or that client's setting changes.
func clipRelayResume() {
	if clipEnabled() {
		clipRelayState(clipOn)
	} else {
		clipRelayState(clipOff)
	}
}

// spawnHelper starts the helper as the person signed in to our session.
func spawnHelper(port int, token string) (syscall.Handle, error) {
	var session uint32
	if r, _, err := pProcessIdToSessionId.Call(uintptr(syscall.Getpid()), uintptr(unsafe.Pointer(&session))); r == 0 {
		return 0, fmt.Errorf("session id: %w", err)
	}
	var tok syscall.Token
	if r, _, err := pWTSQueryUserToken.Call(uintptr(session), uintptr(unsafe.Pointer(&tok))); r == 0 {
		return 0, fmt.Errorf("no signed-in user token: %w", err)
	}
	defer tok.Close()
	// Give it that person's own environment (TEMP, APPDATA...) if we can.
	var env unsafe.Pointer
	flags := uint32(0x08000000) // CREATE_NO_WINDOW
	if r, _, _ := pCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&env)), uintptr(tok), 0); r != 0 && env != nil {
		defer pDestroyEnvironmentBlock.Call(uintptr(env))
		flags |= 0x00000400 // CREATE_UNICODE_ENVIRONMENT
	} else {
		env = nil
	}
	exe, _ := os.Executable()
	si := syscall.StartupInfo{Desktop: u16(`winsta0\default`)}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	cmd := `"` + exe + `" cliphelper ` + strconv.Itoa(port) + " " + token
	if err := syscall.CreateProcessAsUser(tok, nil, u16(cmd), nil, nil, false, flags,
		(*uint16)(env), u16(filepath.Dir(exe)), &si, &pi); err != nil {
		return 0, fmt.Errorf("create process: %w", err)
	}
	syscall.CloseHandle(pi.Thread)
	return pi.Process, nil
}

// relayPeer is a session's clipboard connection when the helper does the work.
type relayPeer struct{ s *session }

func (r relayPeer) Remote(t byte, p []byte) {
	// Only the client in control shares copy and paste.
	if h := helper.Load(); h != nil && inControl(r.s) && r.s.clipOn.Load() && t != proto.MsgClipState {
		h.Send(t, true, p)
	}
}

// Close has nothing to undo: when the client in control goes, clipControl
// moves sharing on or switches it off.
func (r relayPeer) Close() {}

// ---------- helper side ----------

// runClipHelper is "deskbridge-server.exe cliphelper PORT TOKEN". It exits
// when the worker goes away.
func runClipHelper(args []string) {
	if len(args) < 2 {
		return
	}
	nc, err := net.DialTimeout("tcp", "127.0.0.1:"+args[0], 5*time.Second)
	if err != nil {
		return
	}
	c := proto.NewConn(nc)
	if c.Send(msgHelperHello, true, []byte(args[1])) != nil {
		return
	}
	var enabled atomic.Bool
	sync := &winclip.Sync{Enabled: enabled.Load}
	if !sync.Start() {
		return
	}
	send := func(t byte, p []byte) error { return c.Send(t, true, p) }
	var peer *winclip.Peer
	for {
		t, p, err := c.Read()
		if err != nil {
			if peer != nil {
				peer.Close()
			}
			sync.Shutdown()
			return
		}
		switch {
		case t == proto.MsgClipState:
			on := len(p) > 0 && p[0] == clipOn
			enabled.Store(on)
			if on && peer == nil {
				peer = sync.Connect(send)
			} else if !on && peer != nil {
				peer.Close()
				peer = nil
			}
		case peer != nil && proto.IsClipMsg(t):
			peer.Remote(t, p)
		}
	}
}

//go:build windows

// deskbridge-server shares the signed-in Windows desktop (screen, mouse,
// keyboard, audio) with deskbridge-client over the local network.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/pace"
	"deskbridge/internal/proto"
	"deskbridge/internal/winclip"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")

	pSetDpiCtx        = user32.NewProc("SetProcessDpiAwarenessContext")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pEnumMonitors     = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfo   = user32.NewProc("GetMonitorInfoW")
	pLockWorkStation  = user32.NewProc("LockWorkStation")
	pExitWindowsEx    = user32.NewProc("ExitWindowsEx")
	pGetDC            = user32.NewProc("GetDC")
	pReleaseDC        = user32.NewProc("ReleaseDC")
	pSendInput        = user32.NewProc("SendInput")
	pGetCursorInfo    = user32.NewProc("GetCursorInfo")
	pLoadCursor       = user32.NewProc("LoadCursorW")
	pOpenInputDesktop = user32.NewProc("OpenInputDesktop")
	pGetThreadDesktop = user32.NewProc("GetThreadDesktop")
	pSetThreadDesktop = user32.NewProc("SetThreadDesktop")
	pCloseDesktop     = user32.NewProc("CloseDesktop")
	pGetUserObjInfo   = user32.NewProc("GetUserObjectInformationW")

	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pBitBlt             = gdi32.NewProc("BitBlt")
	pGdiFlush           = gdi32.NewProc("GdiFlush")

	pSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
	pGetCurrentThreadId      = kernel32.NewProc("GetCurrentThreadId")

	pCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	pCoCreateInstance = ole32.NewProc("CoCreateInstance")
	pCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

var (
	flagPort    = flag.String("port", proto.DefaultPort, "TCP port to listen on")
	flagPass    = flag.String("password", "", "password (default: generated and saved next to the exe)")
	flagQuality = flag.Int("quality", 85, "JPEG quality for photo/video areas (1-100)")
	flagFPS     = flag.Int("fps", 30, "maximum frames per second")
	flagNoAudio = flag.Bool("noaudio", false, "do not send audio")
	flagSleep   = flag.Bool("allowsleep", false, "let the PC go to sleep while the server runs")
)

const passwordFile = "deskbridge-password.txt"

// workerMode is set when the service started this process (no console).
var workerMode bool

type session struct {
	c           *proto.Conn
	pending     atomic.Int32
	link        pace.Meter   // how much the connection carries, learned from acknowledgements
	fps         atomic.Int32 // these three can be changed live by the client
	quality     atomic.Int32
	audioOn     atomic.Bool
	clipOn      atomic.Bool
	videoOn     atomic.Bool   // client wants motion sent as video
	codecs      atomic.Uint32 // which video codecs the client can decode
	restart     atomic.Bool   // the client asked for a fresh video stream
	fastCapture atomic.Bool
	monitor     atomic.Int32   // which display to show; -1 for all of them
	heard       atomic.Int64   // when anything last arrived from the client
	pinged      atomic.Bool    // the client sends pings, so silence means it is gone
	update      updateReceiver // used only by the input goroutine
	done        chan struct{}
	once        sync.Once

	remote string   // the client's address, for messages
	peer   clipPeer // its side of copy and paste

	// The part of the desktop this client is shown (one display, or all
	// of them), in desktop coordinates. Its mouse positions are relative
	// to it.
	regionX, regionY, regionW, regionH atomic.Int32
	device                             atomic.Pointer[string] // the display it is shown; "" for all displays

	// What it is holding down, so it can be let go if control moves on.
	heldMu       sync.Mutex
	heldKeys     map[uint32]keybdInput
	heldButtons  uint32
	lastX, lastY int
}

func (s *session) close() {
	s.once.Do(func() { close(s.done); s.c.Close() })
}

func (s *session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

var (
	// allowSleep starts from the -allowsleep flag; a client can change it,
	// and the choice then holds until the server restarts.
	allowSleep atomic.Bool
)

func main() {
	if len(os.Args) > 1 {
		for _, a := range os.Args[2:] {
			quiet = quiet || a == "quiet"
		}
		switch os.Args[1] {
		case "install", "uninstall":
			if os.Args[1] == "install" {
				install()
			} else {
				uninstall()
			}
			if quiet && !installOK {
				os.Exit(1)
			}
			return
		case "service":
			runService()
			return
		case "cliphelper":
			runClipHelper(os.Args[2:])
			return
		case "worker":
			workerMode = true
			os.Args = append(os.Args[:1], os.Args[2:]...)
			logToFile()
		}
	}
	flag.Parse()
	runtime.LockOSThread()
	pSetDpiCtx.Call(^uintptr(3)) // per-monitor v2: capture and inject in real pixels

	password := *flagPass
	if password == "" {
		password = loadOrCreatePassword()
	}
	key := proto.DeriveKey(password)
	cfg, err := proto.NewServerTLS()
	if err != nil {
		fatal(err)
	}
	ln, err := net.Listen("tcp", ":"+*flagPort)
	if err != nil {
		fatal(err)
	}
	allowSleep.Store(*flagSleep)
	go powerLoop()
	startClipboard()
	startRelay(func(nc net.Conn) { serve(nc, cfg, key) })
	if workerMode {
		log.Printf("worker %s listening on port %s", proto.Version, *flagPort)
		for {
			nc, err := ln.Accept()
			if err != nil {
				fatal(err)
			}
			go serve(nc, cfg, key)
		}
	}

	host, _ := os.Hostname()
	fmt.Println("Deskbridge server", proto.Version, "is running.")
	fmt.Println()
	fmt.Println("  Connect to:", host)
	for _, ip := range localIPs() {
		fmt.Println("          or:", ip)
	}
	fmt.Println("  Port:      ", *flagPort)
	fmt.Println("  Password:  ", password)
	fmt.Println()
	fmt.Println("Leave this window open. Close it to stop sharing.")
	fmt.Println()

	for {
		nc, err := ln.Accept()
		if err != nil {
			fatal(err)
		}
		go serve(nc, cfg, key)
	}
}

// powerLoop keeps the PC from idling to sleep so it stays reachable, and
// keeps the display on only while someone is connected.
func powerLoop() {
	runtime.LockOSThread()
	last := uintptr(0)
	for {
		want := uintptr(0x80000000) // ES_CONTINUOUS alone: no requirements
		if !allowSleep.Load() {
			want |= 0x1 // ES_SYSTEM_REQUIRED
			if connected() {
				want |= 0x2 // ES_DISPLAY_REQUIRED
			}
		}
		if want != last {
			pSetThreadExecutionState.Call(want)
			last = want
		}
		time.Sleep(time.Second)
	}
}

func fatal(err error) {
	if workerMode {
		log.Print("fatal: ", err)
		os.Exit(1)
	}
	fmt.Println("Error:", err)
	fmt.Println("Press Enter to exit.")
	fmt.Scanln()
	os.Exit(1)
}

func loadOrCreatePassword() string {
	exe, _ := os.Executable()
	return loadOrCreatePasswordAt(filepath.Join(filepath.Dir(exe), passwordFile))
}

func loadOrCreatePasswordAt(path string) string {
	if b, err := os.ReadFile(path); err == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			return p
		}
	}
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	raw := make([]byte, 12)
	rand.Read(raw)
	var sb strings.Builder
	for i, b := range raw {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	if err := os.WriteFile(path, []byte(sb.String()+"\r\n"), 0600); err != nil {
		fmt.Println("Note: could not save the password file, so a new password will be made next time:", err)
	}
	return sb.String()
}

func localIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP.String())
		}
	}
	return out
}

func serve(nc net.Conn, cfg *tls.Config, key []byte) {
	remote := nc.RemoteAddr().String()
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	tc, role, err := proto.ServerHandshake(nc, cfg, key)
	if err != nil {
		log.Printf("%s: rejected (%v)", remote, err)
		nc.Close()
		return
	}
	if role == proto.RoleMaintenance {
		serveMaintenance(tc, remote)
		return
	}
	s := &session{c: proto.NewConn(tc), done: make(chan struct{}), remote: hostOf(remote)}
	s.fps.Store(int32(*flagFPS))
	s.quality.Store(int32(*flagQuality))
	s.audioOn.Store(true)
	s.clipOn.Store(true)
	s.fastCapture.Store(true)
	s.peer = clipAttach(s)
	// It joins whoever is connected already (see control.go) and is told
	// whether it is in control before anything else is sent.
	addSession(s)
	log.Printf("%s: connected", remote)
	s.c.Send(proto.MsgRelayInfo, true, relayInfo())

	go videoLoop(s)
	if !*flagNoAudio {
		go audioLoop(s)
	}
	inputLoop(s, s.peer)
	s.close()
	s.peer.Close()
	removeSession(s)
	restoreResolutionsLater()
	log.Printf("%s: disconnected", remote)
}

// serveMaintenance answers a client that only wants to know this server's
// version and, if it is behind, hand it an update. Nothing is shown or
// controlled, and whoever is using the PC remotely is left alone (until an
// update restarts the server, which they then ride out by reconnecting).
func serveMaintenance(tc *tls.Conn, remote string) {
	s := &session{c: proto.NewConn(tc), done: make(chan struct{})}
	defer s.close()
	log.Printf("%s: maintenance connection", remote)
	s.c.Send(proto.MsgNotice, true, append([]byte{proto.NoticeVersion}, proto.Version...))
	for {
		tc.SetReadDeadline(time.Now().Add(2 * time.Minute))
		t, p, err := s.c.Read()
		if err != nil {
			return
		}
		switch t {
		case proto.MsgPing:
			s.c.Send(proto.MsgPong, true)
		case proto.MsgUpdateBegin, proto.MsgUpdateData, proto.MsgUpdateEnd:
			s.update.handle(s, t, p)
		}
	}
}

// ---------- clipboard ----------
//
// Run by hand, the server shares the clipboard itself. As a service it
// cannot: its worker runs as SYSTEM, and the clipboard (above all, files
// pasted into File Explorer) has to be handled by a program running as the
// signed-in person. So the worker starts a small helper as that person and
// passes clipboard messages between it and the client. See cliphelper.go.

var clip *winclip.Sync // nil in worker mode or if unavailable

type clipPeer interface {
	Remote(t byte, p []byte)
	Close()
}

type noClip struct{}

func (noClip) Remote(byte, []byte) {}
func (noClip) Close()              {}

// clipEnabled reports whether the client in control has sharing on.
func clipEnabled() bool {
	s := control.Load()
	return s != nil && !s.closed() && s.clipOn.Load()
}

func startClipboard() {
	if workerMode {
		// Earlier versions kept pasted files here; nothing does any more.
		if pd := os.Getenv("ProgramData"); pd != "" {
			os.RemoveAll(filepath.Join(pd, "Deskbridge", "Clipboard"))
		}
		startClipRelay()
		return
	}
	c := &winclip.Sync{Enabled: clipEnabled, Status: func(msg string) { log.Print("clipboard: ", msg) }}
	if !c.Start() {
		log.Print("clipboard sharing unavailable: could not create the clipboard window")
		return
	}
	clip = c
}

// clipAttach makes a new session's side of copy and paste. Sharing is
// with one client at a time, the one in control: see clipControl.
func clipAttach(s *session) clipPeer {
	switch {
	case workerMode:
		return relayPeer{s}
	case clip != nil:
		return clip.NewPeer(func(t byte, p []byte) error { return s.c.Send(t, true, p) })
	}
	return noClip{}
}

// ---------- desktop tracking ----------

// Windows shows the sign-in screen, the lock screen and admin approval
// prompts on a separate "secure" desktop. A thread only captures and injects
// into the desktop it is attached to, so each thread that does either follows
// the desktop that is currently receiving input. Opening the secure desktop
// needs SYSTEM rights, which the service worker has; when run by hand the
// open simply fails and the thread stays on the normal desktop.
type deskState struct {
	handle    uintptr // desktop we switched to, 0 while still on the original
	name      string
	failUntil time.Time
}

func desktopName(h uintptr) string {
	var buf [256]uint16
	var n uint32
	pGetUserObjInfo.Call(h, 2, uintptr(unsafe.Pointer(&buf[0])), unsafe.Sizeof(buf), uintptr(unsafe.Pointer(&n)))
	return syscall.UTF16ToString(buf[:])
}

func newDeskState() *deskState {
	tid, _, _ := pGetCurrentThreadId.Call()
	h, _, _ := pGetThreadDesktop.Call(tid)
	return &deskState{name: desktopName(h)}
}

// sync attaches the calling (locked) thread to the input desktop if that
// has changed. before runs just ahead of a switch, to drop anything tied to
// the old desktop.
func (d *deskState) sync(before func()) bool {
	if time.Now().Before(d.failUntil) {
		return false
	}
	h, _, _ := pOpenInputDesktop.Call(0, 0, 0x10000000) // GENERIC_ALL
	if h == 0 {
		return false
	}
	name := desktopName(h)
	if name == d.name {
		pCloseDesktop.Call(h)
		return false
	}
	if before != nil {
		before()
	}
	if r, _, _ := pSetThreadDesktop.Call(h); r == 0 {
		pCloseDesktop.Call(h)
		d.failUntil = time.Now().Add(time.Second) // do not retry on every frame
		return false
	}
	if d.handle != 0 {
		pCloseDesktop.Call(d.handle)
	}
	d.handle, d.name = h, name
	return true
}

// ---------- input ----------

type mouseInput struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	_         uint32
	extra     uintptr
}

type keybdInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	_     uint32
	extra uintptr
	_     [8]byte
}

const (
	mouseMove        = 0x0001
	mouseAbsolute    = 0x8000
	mouseVirtualDesk = 0x4000 // positions span every display, not just the main one
)

// sendMouse moves the pointer to (x, y) on the part of the desktop a client
// is shown, with the given button or wheel event.
func sendMouse(s *session, x, y int, flags, data uint32) {
	rw, rh := int(s.regionW.Load()), int(s.regionH.Load())
	// The whole desktop, across every display: positions are given as
	// fractions of it. Its corner is negative when a display sits left of or
	// above the main one.
	metric := func(i uintptr) int { v, _, _ := pGetSystemMetrics.Call(i); return int(int32(v)) }
	vx, vy, vw, vh := metric(76), metric(77), metric(78), metric(79)
	if rw < 1 || rh < 1 || vw < 2 || vh < 2 {
		return
	}
	px := int(s.regionX.Load()) + min(max(x, 0), rw-1)
	py := int(s.regionY.Load()) + min(max(y, 0), rh-1)
	in := mouseInput{
		typ:       0,
		dx:        int32((px - vx) * 65535 / (vw - 1)),
		dy:        int32((py - vy) * 65535 / (vh - 1)),
		mouseData: data,
		flags:     flags | mouseMove | mouseAbsolute | mouseVirtualDesk,
	}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func inputLoop(s *session, peer clipPeer) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	desk := newDeskState()
	desk.sync(nil)
	lastSync := time.Now()
	// Whatever it still holds down when it goes is let go, on this thread.
	defer s.releaseHeld()
	// Nudge the pointer so a sleeping display wakes up for the new viewer.
	for _, dx := range []int32{1, -1} {
		in := mouseInput{dx: dx, flags: mouseMove}
		pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	}
	lastX, lastY := 0, 0
	for {
		t, p, err := s.c.Read()
		if err != nil {
			return
		}
		if proto.IsClipMsg(t) {
			peer.Remote(t, p)
			continue
		}
		s.heard.Store(time.Now().UnixNano())
		if t == proto.MsgPing {
			s.pinged.Store(true)
			s.c.Send(proto.MsgPong, true)
			continue
		}
		if t != proto.MsgAck && time.Since(lastSync) > 100*time.Millisecond {
			desk.sync(nil)
			lastSync = time.Now()
		}
		switch t {
		case proto.MsgAck:
			s.pending.Add(-1)
			s.link.Acked(time.Now())
		case proto.MsgSettings:
			if len(p) < 4 {
				continue
			}
			s.fps.Store(int32(p[0]))
			s.quality.Store(int32(p[1]))
			s.audioOn.Store(p[2] != 0)
			allowSleep.Store(p[3] != 0)
			if len(p) >= 8 {
				s.fastCapture.Store(p[7] != 0)
			}
			if len(p) >= 7 {
				s.videoOn.Store(p[5] != 0)
				s.codecs.Store(uint32(p[6]))
			}
			if len(p) >= 5 {
				s.clipOn.Store(p[4] != 0)
				if workerMode && inControl(s) {
					clipRelayResume()
				}
			}
		case proto.MsgSelectMonitor:
			if len(p) >= 1 {
				s.monitor.Store(int32(int8(p[0])))
			}
		case proto.MsgSetResolution:
			if len(p) >= 4 {
				// The resolution is the PC's own, shared by everyone
				// watching, so only the client in control may change it.
				if !inControl(s) {
					tell(s, "The resolution was not changed: another PC is in control of this one. Take control first.")
					continue
				}
				go changeResolution(s, int(binary.LittleEndian.Uint16(p)), int(binary.LittleEndian.Uint16(p[2:])))
			}
		case proto.MsgTakeControl:
			takeControl(s)
		case proto.MsgRelaySet:
			// From a client that has given this PC's password, so it may
			// decide how this PC can be reached.
			address, key, _ := strings.Cut(string(p), "\n")
			if len(p) > 600 {
				address, key = "?", ""
			}
			if err := setRelay(strings.TrimSpace(address), strings.TrimSpace(key)); err != nil {
				s.c.Send(proto.MsgNotice, true, append([]byte{proto.NoticeGeneral}, "Internet access was not set up on the remote PC: "+err.Error()+"."...))
			}
			s.c.Send(proto.MsgRelayInfo, true, relayInfo())
		case proto.MsgAction:
			if len(p) >= 1 {
				takeControl(s)
				doAction(s, p[0])
			}
		case proto.MsgUpdateBegin, proto.MsgUpdateData, proto.MsgUpdateEnd:
			s.update.handle(s, t, p)
		case proto.MsgVideoRestart:
			s.restart.Store(true)
		case proto.MsgMouseMove:
			if len(p) < 4 {
				continue
			}
			lastX, lastY = int(binary.LittleEndian.Uint16(p)), int(binary.LittleEndian.Uint16(p[2:]))
			// Moving the pointer does not take control: a client that is
			// only looking must not pull it away from the one working.
			if inControl(s) {
				sendMouse(s, lastX, lastY, 0, 0)
			}
		case proto.MsgMouseBtn:
			if len(p) < 6 || p[0] > 4 {
				continue
			}
			lastX, lastY = int(binary.LittleEndian.Uint16(p[2:])), int(binary.LittleEndian.Uint16(p[4:]))
			down := 0
			if p[1] != 0 {
				down = 1
				takeControl(s) // a click takes control
			}
			if !inControl(s) {
				continue // the release of a press made before control moved on
			}
			s.buttonChange(p[0], down == 1, lastX, lastY)
			sendMouse(s, lastX, lastY, buttonFlags[p[0]][down], buttonData(p[0]))
		case proto.MsgWheel:
			if len(p) < 3 {
				continue
			}
			takeControl(s) // so does scrolling
			delta := int32(int16(binary.LittleEndian.Uint16(p)))
			flags := uint32(0x0800)
			if p[2] != 0 {
				flags = 0x1000
			}
			sendMouse(s, lastX, lastY, flags, uint32(delta))
		case proto.MsgKey:
			if len(p) < 5 {
				continue
			}
			up := p[4]&2 != 0
			if !up {
				takeControl(s) // and so does typing
			} else if !inControl(s) {
				continue
			}
			in := keybdInput{typ: 1}
			vk, scan := binary.LittleEndian.Uint16(p), binary.LittleEndian.Uint16(p[2:])
			if scan != 0 {
				in.scan = scan
				in.flags = 0x0008 // KEYEVENTF_SCANCODE
			} else {
				in.vk = vk
			}
			if p[4]&1 != 0 {
				in.flags |= 0x0001 // KEYEVENTF_EXTENDEDKEY
			}
			id := uint32(vk) | uint32(scan)<<16
			if up {
				in.flags |= 0x0002 // KEYEVENTF_KEYUP
				s.keyUp(id)
			} else {
				release := in
				release.flags |= 0x0002
				s.keyDown(id, release)
			}
			pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
		}
	}
}

// ---------- screen ----------

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type cursorInfo struct {
	size   uint32
	flags  uint32
	handle uintptr
	x, y   int32
}

var cursorIDs = []uint16{32512, 32513, 32514, 32515, 32516, 32642, 32643, 32644, 32645, 32646, 32648, 32649, 32650, 32651}

func videoLoop(s *session) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer s.close()

	cursors := map[uintptr]uint16{}
	for _, id := range cursorIDs {
		if h, _, _ := pLoadCursor.Call(0, uintptr(id)); h != 0 {
			cursors[h] = id
		}
	}
	lastCursor := uint16(0)
	lastPointer := [3]int{-1, -1, -1} // the pointer position last sent: x, y, visible

	var c capturer
	defer c.free()
	desk := newDeskState()
	var prev []byte
	vid := newMotionVideo(s)
	defer vid.close()
	recheck := false // compare with the last frame even if the screen has not changed
	// For a link slower than a local network (see internal/pace): tiles
	// knowingly left unsent, to follow in the next frames; how many bytes a
	// pixel of tiles has been coming to; what the client was last told.
	owed := map[[2]int]struct{}{}
	perPixel := 0.25
	var toldLink []byte
	var toldLinkAt time.Time
	toldFast := false       // which capture method the client was last told about
	var toldMonitors []byte // the display list the client was last sent
	s.c.Send(proto.MsgNotice, false, append([]byte{proto.NoticeVersion}, proto.Version...))
	s.c.Send(proto.MsgNotice, false, append([]byte{proto.NoticeCapture}, "Screen capture: standard."...))
	sem := make(chan struct{}, runtime.NumCPU())
	u16 := func(v int) []byte { return binary.LittleEndian.AppendUint16(nil, uint16(v)) }

	for !s.closed() {
		// A client that pings and then falls silent has gone away (asleep,
		// unplugged, off the network) without the connection noticing.
		if s.pinged.Load() && time.Since(time.Unix(0, s.heard.Load())) > 15*time.Second {
			log.Print("client stopped answering; closing the connection")
			return
		}
		interval := time.Second / time.Duration(min(max(s.fps.Load(), 1), 120))
		// Two frames on their way at once, or a few more when the round
		// trip is long.
		if s.pending.Load() >= s.link.Window(interval) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		start := time.Now()
		quality := int(min(max(s.quality.Load(), 1), 100))
		if start.Sub(toldLinkAt) >= 2*time.Second {
			toldLinkAt = start
			rate, roundTrip := s.link.Info()
			info := binary.LittleEndian.AppendUint32(nil, uint32(rate/1000))
			info = binary.LittleEndian.AppendUint16(info, uint16(min(roundTrip/time.Millisecond, 60000)))
			if !bytes.Equal(info, toldLink) {
				toldLink = info
				s.c.Send(proto.MsgLinkInfo, false, info)
			}
		}
		desk.sync(c.free) // freeing forces a fresh capture surface and a full frame
		choice := int(s.monitor.Load())
		if desk.name != "" && !strings.EqualFold(desk.name, "Default") {
			// The lock screen or an admin prompt is showing. Those can be on
			// a display other than the one being watched, and cannot be
			// answered unseen, so show every display until it is gone.
			choice = -1
		}
		changed, ok := c.ensure(choice)
		if sig := c.describe(); ok && !bytes.Equal(sig, toldMonitors) {
			toldMonitors = sig
			s.c.Send(proto.MsgMonitors, false, sig)
		}
		same := false
		if ok {
			same, ok = c.grab(s.fastCapture.Load(), int(min(interval, 40*time.Millisecond)/time.Millisecond))
		}
		if ok && c.fast != toldFast {
			toldFast = c.fast
			note := "Screen capture: standard."
			if c.fast {
				note = "Screen capture: fast."
			}
			s.c.Send(proto.MsgNotice, false, append([]byte{proto.NoticeCapture}, note...))
		}
		if !ok {
			// On a secure desktop we cannot open (not running as the
			// service), or mid display change.
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if changed {
			prev = nil
			s.regionX.Store(int32(c.x))
			s.regionY.Store(int32(c.y))
			s.regionW.Store(int32(c.w))
			s.regionH.Store(int32(c.h))
			sz := binary.LittleEndian.AppendUint32(nil, uint32(c.w))
			sz = binary.LittleEndian.AppendUint32(sz, uint32(c.h))
			if s.c.Send(proto.MsgSize, false, sz) != nil {
				return
			}
			// Which display this is, and the resolutions it could be set to.
			device := ""
			if c.selected >= 0 && len(c.parts) == 1 {
				device = c.parts[0].device
			}
			s.device.Store(&device)
			s.c.Send(proto.MsgModes, false, describeModes(device, c.w, c.h))
		}

		ci := cursorInfo{size: uint32(unsafe.Sizeof(cursorInfo{}))}
		if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r != 0 {
			id, known := cursors[ci.handle]
			if !known {
				id = 32512
			}
			if id != lastCursor {
				lastCursor = id
				if s.c.Send(proto.MsgCursor, true, u16(int(id))) != nil {
					return
				}
			}
			// A client that is not the one in control cannot tell where
			// the pointer is from the picture, which leaves it out: say.
			if !inControl(s) {
				px, py := int(ci.x)-c.x, int(ci.y)-c.y
				at := [3]int{px, py, 0}
				if ci.flags&1 != 0 && px >= 0 && py >= 0 && px < c.w && py < c.h { // CURSOR_SHOWING, and on the part shown
					at[2] = 1
				} else {
					at[0], at[1] = 0, 0
				}
				if at != lastPointer {
					lastPointer = at
					if s.c.Send(proto.MsgPointer, true, u16(at[0]), u16(at[1]), []byte{byte(at[2])}) != nil {
						return
					}
				}
			} else {
				lastPointer = [3]int{-1, -1, -1} // send afresh if control moves on
			}
		}

		var rects []proto.Rect
		if !same || prev == nil || recheck {
			// Fast capture already knows when nothing changed; otherwise
			// compare with the last frame sent.
			rects = proto.DirtyRects(c.pix, prev, c.w, c.h, 64)
			recheck = false
		}
		// How much changed, leaving out what is only waiting its turn to
		// be sent: that is not motion.
		area := 0
		for _, r := range rects {
			for x := r.X; x < r.X+r.W; x += tile {
				if _, waiting := owed[[2]int{x / tile, r.Y / tile}]; !waiting {
					area += min(tile, r.X+r.W-x) * r.H
				}
			}
		}
		switch vid.decide(area, c.w, c.h, prev == nil, interval, quality) {
		case leaveVideo:
			for t := range vid.touched {
				owed[t] = struct{}{}
			}
			vid.markForRedraw(prev, c.w) // the next pass redraws, crisply, what video drew
			recheck = true               // even if the screen itself does not change again
			continue
		case useVideo:
			clear(owed) // video draws the whole picture
			if len(rects) > 0 {
				sent, err := vid.send(c.pix, c.w, rects)
				if err != nil {
					if s.closed() {
						return
					}
					prev = nil
					continue
				}
				copy(prev, c.pix)
				if sent {
					s.link.Sent(vid.wroteAt, vid.wroteBytes)
					s.pending.Add(1)
					if s.c.Send(proto.MsgFrameEnd, true) != nil {
						return
					}
				}
			}
			if d := interval - time.Since(start); d > 0 {
				time.Sleep(d)
			}
			continue
		}
		clear(owed)
		if len(rects) > 0 {
			type enc struct {
				kind byte
				data []byte
			}
			// On a slow link a large change goes in pieces, each small
			// enough to cross in about a tenth of a second, top of the
			// screen first. The rest stays marked as changed and follows in
			// the next frames, so the picture keeps moving and the pointer
			// and keyboard stay responsive, where one huge frame would
			// hold everything up until it had arrived.
			all := rects
			budget := s.link.Budget()
			if budget > 0 {
				n, guess := 0, 0.0
				for n < len(rects) && (n == 0 || guess < float64(budget)) {
					guess += float64(rects[n].W*rects[n].H) * perPixel
					n++
				}
				rects = rects[:n]
			}
			tileQuality := s.link.Quality(quality)
			out := make([]enc, len(rects))
			var wg sync.WaitGroup
			for i, r := range rects {
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer func() { <-sem; wg.Done() }()
					out[i].kind, out[i].data = proto.EncodeRect(c.pix, c.w, r, tileQuality)
				}()
			}
			wg.Wait()
			size, pixels := 0, 0
			for i, r := range rects {
				if budget > 0 && i > 0 && int64(size) >= budget*5/4 {
					rects, out = rects[:i], out[:i] // the guess was too low
					break
				}
				size += len(out[i].data)
				pixels += r.W * r.H
			}
			perPixel = min(max(perPixel+0.3*(float64(size)/float64(max(pixels, 1))-perPixel), 0.005), 3)
			if prev == nil {
				// All of it counts as sent, then what was held back is
				// marked as still to send.
				prev = make([]byte, len(c.pix))
				copy(prev, c.pix)
				for _, r := range all[len(rects):] {
					for x := r.X; x < r.X+r.W; x += tile {
						prev[(r.Y*c.w+x)*4] ^= 0xFF
					}
				}
			} else {
				for _, r := range rects {
					for y := r.Y; y < r.Y+r.H; y++ {
						o := (y*c.w + r.X) * 4
						copy(prev[o:o+r.W*4], c.pix[o:o+r.W*4])
					}
				}
			}
			if len(rects) < len(all) {
				for _, r := range all[len(rects):] {
					for x := r.X; x < r.X+r.W; x += tile {
						owed[[2]int{x / tile, r.Y / tile}] = struct{}{}
					}
				}
				recheck = true // even if the screen itself does not change again
			}
			wrote, before := time.Now(), s.c.Sent()
			for i, r := range rects {
				hdr := make([]byte, 9)
				binary.LittleEndian.PutUint16(hdr[0:], uint16(r.X))
				binary.LittleEndian.PutUint16(hdr[2:], uint16(r.Y))
				binary.LittleEndian.PutUint16(hdr[4:], uint16(r.W))
				binary.LittleEndian.PutUint16(hdr[6:], uint16(r.H))
				hdr[8] = out[i].kind
				if s.c.Send(proto.MsgRect, false, hdr, out[i].data) != nil {
					return
				}
			}
			s.link.Sent(wrote, s.c.Sent()-before)
			s.pending.Add(1)
			if s.c.Send(proto.MsgFrameEnd, true) != nil {
				return
			}
		}
		if d := interval - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
}

// ---------- audio (WASAPI loopback) ----------

type guid struct {
	d1 uint32
	d2 uint16
	d3 uint16
	d4 [8]byte
}

var (
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = guid{0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = guid{0xC8ADBD64, 0xE71E, 0x48A0, [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

// vcall invokes method number idx on a COM object.
func vcall(obj uintptr, idx int, args ...uintptr) error {
	vtbl := *(*unsafe.Pointer)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	if int32(hr) < 0 {
		return fmt.Errorf("HRESULT 0x%08X", uint32(hr))
	}
	return nil
}

func release(obj uintptr) {
	if obj != 0 {
		vcall(obj, 2)
	}
}

func audioLoop(s *session) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pCoInitializeEx.Call(0, 0) // multithreaded apartment
	logged := false
	for !s.closed() {
		err := captureAudio(s)
		if s.closed() {
			return
		}
		if err != nil && !logged {
			log.Printf("audio unavailable, will keep retrying: %v", err)
			logged = true
		}
		time.Sleep(2 * time.Second)
	}
}

func captureAudio(s *session) error {
	var enum, dev, client, capt uintptr
	defer func() { release(capt); release(client); release(dev); release(enum) }()

	hr, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, 23,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum)))
	if int32(hr) < 0 {
		return fmt.Errorf("create device enumerator: 0x%08X", uint32(hr))
	}
	// GetDefaultAudioEndpoint(eRender, eConsole)
	if err := vcall(enum, 4, 0, 0, uintptr(unsafe.Pointer(&dev))); err != nil {
		return fmt.Errorf("no default playback device: %w", err)
	}
	activate := func() error {
		release(client)
		client = 0
		return vcall(dev, 3, uintptr(unsafe.Pointer(&iidIAudioClient)), 23, 0, uintptr(unsafe.Pointer(&client)))
	}
	if err := activate(); err != nil {
		return fmt.Errorf("activate audio client: %w", err)
	}

	const loopback, autoConvert, srcQuality = 0x00020000, 0x80000000, 0x08000000
	const bufferDuration = 2000000 // 200 ms in 100 ns units
	rate, channels, bits, isFloat := 48000, 2, 16, false

	// First choice: let Windows convert to 48 kHz 16-bit stereo for us.
	want := waveFormatEx{FormatTag: 1, Channels: 2, SamplesPerSec: 48000, AvgBytesPerSec: 192000, BlockAlign: 4, BitsPerSample: 16}
	err := vcall(client, 3, 0, loopback|autoConvert|srcQuality, bufferDuration, 0, uintptr(unsafe.Pointer(&want)), 0)
	if err != nil {
		// Fallback: capture in the device's own format and convert here.
		if err := activate(); err != nil {
			return err
		}
		var mix *waveFormatEx
		if err := vcall(client, 8, uintptr(unsafe.Pointer(&mix))); err != nil {
			return fmt.Errorf("get mix format: %w", err)
		}
		rate, channels, bits = int(mix.SamplesPerSec), int(mix.Channels), int(mix.BitsPerSample)
		tag := uint32(mix.FormatTag)
		if mix.FormatTag == 0xFFFE && mix.Size >= 22 {
			tag = *(*uint32)(unsafe.Add(unsafe.Pointer(mix), 24)) // SubFormat.Data1
		}
		isFloat = tag == 3
		err = vcall(client, 3, 0, loopback, bufferDuration, 0, uintptr(unsafe.Pointer(mix)), 0)
		pCoTaskMemFree.Call(uintptr(unsafe.Pointer(mix)))
		if err != nil {
			return fmt.Errorf("initialize loopback capture: %w", err)
		}
		if channels < 1 || (bits != 16 && bits != 24 && bits != 32) {
			return fmt.Errorf("unsupported device format (%d channels, %d bits)", channels, bits)
		}
	}
	if err := vcall(client, 14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&capt))); err != nil {
		return fmt.Errorf("get capture service: %w", err)
	}
	if err := vcall(client, 10); err != nil {
		return fmt.Errorf("start capture: %w", err)
	}
	defer vcall(client, 11)

	if s.c.Send(proto.MsgAudioFmt, true, binary.LittleEndian.AppendUint32(nil, uint32(rate))) != nil {
		return nil
	}
	frameBytes := channels * bits / 8
	var out []byte
	for !s.closed() {
		time.Sleep(10 * time.Millisecond)
		out = out[:0]
		for {
			var n uint32
			if err := vcall(capt, 5, uintptr(unsafe.Pointer(&n))); err != nil {
				return err
			}
			if n == 0 {
				break
			}
			var data unsafe.Pointer
			var frames, flags uint32
			if err := vcall(capt, 3, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0); err != nil {
				return err
			}
			if flags&2 != 0 || data == nil { // AUDCLNT_BUFFERFLAGS_SILENT
				out = append(out, make([]byte, int(frames)*4)...)
			} else {
				out = proto.ToStereo16(out, unsafe.Slice((*byte)(data), int(frames)*frameBytes), int(frames), channels, bits, isFloat)
			}
			if err := vcall(capt, 4, uintptr(frames)); err != nil {
				return err
			}
		}
		if len(out) > 0 && s.audioOn.Load() {
			if s.c.Send(proto.MsgAudio, true, out) != nil {
				return nil
			}
		}
	}
	return nil
}

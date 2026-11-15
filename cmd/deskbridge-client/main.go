//go:build windows

// deskbridge-client shows and controls the desktop shared by
// deskbridge-server. One window: a collapsible settings pane on the left and
// the remote screen on the right.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/mf"
	"deskbridge/internal/proto"
	"deskbridge/internal/winclip"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	winmm    = syscall.NewLazyDLL("winmm.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")
	crypt32  = syscall.NewLazyDLL("crypt32.dll")

	pSetDpiCtx           = user32.NewProc("SetProcessDpiAwarenessContext")
	pGetDpiForWindow     = user32.NewProc("GetDpiForWindow")
	pRegisterClassEx     = user32.NewProc("RegisterClassExW")
	pCreateWindowEx      = user32.NewProc("CreateWindowExW")
	pDefWindowProc       = user32.NewProc("DefWindowProcW")
	pShowWindow          = user32.NewProc("ShowWindow")
	pMoveWindow          = user32.NewProc("MoveWindow")
	pEnableWindow        = user32.NewProc("EnableWindow")
	pIsWindowVisible     = user32.NewProc("IsWindowVisible")
	pGetMessage          = user32.NewProc("GetMessageW")
	pIsDialogMessage     = user32.NewProc("IsDialogMessageW")
	pTranslateMessage    = user32.NewProc("TranslateMessage")
	pDispatchMessage     = user32.NewProc("DispatchMessageW")
	pPostMessage         = user32.NewProc("PostMessageW")
	pSendMessage         = user32.NewProc("SendMessageW")
	pPostQuitMessage     = user32.NewProc("PostQuitMessage")
	pBeginPaint          = user32.NewProc("BeginPaint")
	pEndPaint            = user32.NewProc("EndPaint")
	pInvalidateRect      = user32.NewProc("InvalidateRect")
	pGetClientRect       = user32.NewProc("GetClientRect")
	pGetWindowRect       = user32.NewProc("GetWindowRect")
	pLoadCursor          = user32.NewProc("LoadCursorW")
	pLoadIcon            = user32.NewProc("LoadIconW")
	pSetCursor           = user32.NewProc("SetCursor")
	pGetCursorPos        = user32.NewProc("GetCursorPos")
	pSetCapture          = user32.NewProc("SetCapture")
	pReleaseCapture      = user32.NewProc("ReleaseCapture")
	pSetFocus            = user32.NewProc("SetFocus")
	pGetFocus            = user32.NewProc("GetFocus")
	pSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	pCallNextHookEx      = user32.NewProc("CallNextHookEx")
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	pSetWindowText       = user32.NewProc("SetWindowTextW")
	pGetWindowText       = user32.NewProc("GetWindowTextW")
	pGetWindowLongPtr    = user32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtr    = user32.NewProc("SetWindowLongPtrW")
	pSetWindowPos        = user32.NewProc("SetWindowPos")
	pMonitorFromWindow   = user32.NewProc("MonitorFromWindow")
	pGetMonitorInfo      = user32.NewProc("GetMonitorInfoW")
	pScreenToClient      = user32.NewProc("ScreenToClient")
	pDrawText            = user32.NewProc("DrawTextW")

	pStretchDIBits     = gdi32.NewProc("StretchDIBits")
	pSetStretchBltMode = gdi32.NewProc("SetStretchBltMode")
	pSetBrushOrgEx     = gdi32.NewProc("SetBrushOrgEx")
	pPatBlt            = gdi32.NewProc("PatBlt")
	pCreateFont        = gdi32.NewProc("CreateFontW")
	pSelectObject      = gdi32.NewProc("SelectObject")
	pSetTextColor      = gdi32.NewProc("SetTextColor")
	pSetBkMode         = gdi32.NewProc("SetBkMode")

	pGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
	pCreateActCtx    = kernel32.NewProc("CreateActCtxW")
	pActivateActCtx  = kernel32.NewProc("ActivateActCtx")
	pLocalFree       = kernel32.NewProc("LocalFree")

	pInitCommonControls = comctl32.NewProc("InitCommonControls")

	pCryptProtectData   = crypt32.NewProc("CryptProtectData")
	pCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")

	pWaveOutOpen          = winmm.NewProc("waveOutOpen")
	pWaveOutClose         = winmm.NewProc("waveOutClose")
	pWaveOutReset         = winmm.NewProc("waveOutReset")
	pWaveOutPrepareHeader = winmm.NewProc("waveOutPrepareHeader")
	pWaveOutWrite         = winmm.NewProc("waveOutWrite")
)

const (
	wmDestroy     = 0x0002
	wmSize        = 0x0005
	wmActivate    = 0x0006
	wmSetFocus    = 0x0007
	wmKillFocus   = 0x0008
	wmPaint       = 0x000F
	wmEraseBkgnd  = 0x0014
	wmSetCursor   = 0x0020
	wmSetFont     = 0x0030
	wmCommand     = 0x0111
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A
	wmXButtonDown = 0x020B
	wmXButtonUp   = 0x020C
	wmMouseHWheel = 0x020E
	wmApp         = 0x8000

	wmRepaint      = wmApp + 1  // view: a frame finished
	wmFullscreen   = wmApp + 2  // main: Ctrl+Alt+Enter
	wmRemoteSize   = wmApp + 3  // main: remote resolution known/changed
	wmDisconnected = wmApp + 4  // main: wParam = link id
	wmCursor       = wmApp + 5  // view: remote cursor shape changed
	wmConnectDone  = wmApp + 6  // main: background connect finished
	wmStatus       = wmApp + 8  // main: show the text left in statusText
	wmVideoFailed  = wmApp + 9  // main: the video decoder gave up; switch video off
	wmVideoCaps    = wmApp + 10 // main: finished checking which decoders this PC has

	wsOverlappedWindow = 0x00CF0000
	wsPopup            = 0x80000000
	wsVisible          = 0x10000000
	wsMaximize         = 0x01000000
	wsChild            = 0x40000000
	wsClipChildren     = 0x02000000
	wsTabStop          = 0x00010000
	wsVScroll          = 0x00200000
	wsClipSiblings     = 0x04000000

	idConnect    = 1 // IDOK, so Enter in the pane connects
	idRemember   = 101
	idFPS        = 102
	idQuality    = 103
	idAudio      = 104
	idSleep      = 105
	idFullscreen = 106
	idToggle     = 107
	idClipboard  = 108
	idVideo      = 109
	idFast       = 110

	paneWidth   = 264 // at 96 dpi
	toggleWidth = 16
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
	private uint32
}

type rect struct{ left, top, right, bottom int32 }

type paintStruct struct {
	hdc       uintptr
	erase     int32
	rc        rect
	restore   int32
	incUpdate int32
	reserved  [32]byte
}

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

type monitorInfo struct {
	size    uint32
	monitor rect
	work    rect
	flags   uint32
}

type kbdllHook struct {
	vk    uint32
	scan  uint32
	flags uint32
	time  uint32
	extra uintptr
}

// link is one live connection to a server, with everything that belongs
// to it: the remote screen's picture and what the server has said about
// itself. Several can be live at once, one for each connected tab.
type link struct {
	id    uintptr
	c     *proto.Conn
	name  string         // what the connection was called when it was made
	out   chan [2][]byte // {type, payload}: input, may be dropped if stalled
	bulk  chan [2][]byte // clipboard data: never dropped, yields to input
	done  chan struct{}
	once  sync.Once
	heard atomic.Int64 // when anything last arrived from the server
	pong  atomic.Bool  // the server answers pings, so silence means the link is dead

	// The remote screen. Decoders writing disjoint areas share the read
	// lock; the painter and resizes take the write lock.
	fbMu     sync.RWMutex
	fb       []byte
	fbW, fbH int
	cursor   atomic.Uintptr // the remote pointer's shape

	displays     atomic.Pointer[displayList] // the server's latest list of displays
	modes        atomic.Pointer[modeList]    // the resolutions the shown display offers
	version      atomic.Pointer[string]      // the server's version, if it said
	replaced     atomic.Bool                 // the server said another client took our place
	status       atomic.Pointer[string]      // text for the pane, waiting to be collected
	updateResult chan []byte                 // the server's answer to an update

	// With several PCs connected to the same server, one of them has its
	// mouse and keyboard. (A server older than 1.8.0 serves one PC at a
	// time and never says, so a connection starts out in control.)
	inControl  atomic.Bool            // this PC is the one
	controller atomic.Pointer[string] // otherwise, the name of the PC that is
	pointer    atomic.Uint64          // while watching, where the remote pointer is: x, y<<16, visible<<32
	clipSeq    atomic.Uint32          // this PC's clipboard, numbered, when it stopped being in control

	peer *winclip.Peer // this connection's side of clipboard sharing; nil if unavailable
}

func newLink(id uintptr, c *proto.Conn, name string) *link {
	l := &link{id: id, c: c, name: name, out: make(chan [2][]byte, 2048), bulk: make(chan [2][]byte, 2),
		done: make(chan struct{}), updateResult: make(chan []byte, 1)}
	l.heard.Store(time.Now().UnixNano())
	l.cursor.Store(arrow)
	l.inControl.Store(true)
	if clip != nil {
		// Clipboard data is never dropped, but waits its turn behind input.
		l.peer = clip.NewPeer(func(t byte, p []byte) error {
			select {
			case l.bulk <- [2][]byte{{t}, p}:
				return nil
			case <-l.done:
				return errors.New("disconnected")
			}
		})
	}
	return l
}

func (l *link) close() { l.once.Do(func() { close(l.done); l.c.Close() }) }

// postStatus leaves text for the pane about this connection.
func (l *link) postStatus(s string) {
	l.status.Store(&s)
	pPostMessage.Call(hMain, wmStatus, l.id, 0)
}

type connectResult struct {
	tab         uintptr // the tab the attempt was for
	l           *link
	err         string
	badPassword bool
	auto        bool // an attempt made by reconnecting
	target      target
}

var (
	inst, hMain, hView, hFont uintptr
	dpi                       = 96

	hName, hHost, hPort, hPass, hRemember, hConnect uintptr
	hFPS, hQuality, hAudio, hSleep, hClip           uintptr
	hVideo, hFast                                   uintptr
	hVersion                                        uintptr
	statusTop                                       int // where the status text starts, at 96 dpi
	hFullscreen, hStatus, hToggle                   uintptr
	paneCtls                                        []uintptr

	cur        atomic.Pointer[link] // the connection in front; nil when that tab is not connected
	nextLinkID uintptr
	audioOn    atomic.Bool
	clipOn     atomic.Bool
	statusText atomic.Pointer[string]
	videoCaps  atomic.Uint32 // codecs this PC can decode, once known
	capsKnown  atomic.Bool

	arrow uintptr

	// Touched only on the UI thread.
	pressed    = map[uint32]kbdllHook{} // keys held on the remote PC in front
	localDown  = map[uint32]bool{}      // keys whose press was left to this PC
	swallowing = map[uint32]bool{}      // keys of our own shortcuts whose release is still to come
	keysDown   = map[uint32]bool{}      // keys physically held, wherever their presses went
	fullscreen bool
	collapsed  bool
	savedStyle uintptr
	savedRect  rect
	lastFocus  uintptr
	// localKeys is set while the pointer is on the title bar or border, or
	// the window is being moved or sized. Keys then stay on this PC, so
	// window managers that watch for Shift or Win shortcuts still see them.
	localKeys bool
)

var (
	fpsChoices     = []int{10, 15, 20, 30, 45, 60, 90, 120}
	qualityChoices = []int{50, 60, 70, 80, 85, 90, 95}
)

func u16ptr(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
func sc(v int) int            { return v * dpi / 96 }

func setText(h uintptr, s string) { pSetWindowText.Call(h, uintptr(unsafe.Pointer(u16ptr(s)))) }

func getText(h uintptr) string {
	var buf [512]uint16
	pGetWindowText.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return strings.TrimSpace(syscall.UTF16ToString(buf[:]))
}

func checked(h uintptr) bool { r, _, _ := pSendMessage.Call(h, 0x00F0, 0, 0); return r == 1 }
func setChecked(h uintptr, on bool) {
	v := uintptr(0)
	if on {
		v = 1
	}
	pSendMessage.Call(h, 0x00F1, v, 0)
}

func comboValue(h uintptr, choices []int) int {
	i, _, _ := pSendMessage.Call(h, 0x0147, 0, 0) // CB_GETCURSEL
	if int(i) >= 0 && int(i) < len(choices) {
		return choices[i]
	}
	return choices[len(choices)/2]
}

func setCombo(h uintptr, choices []int, value int) {
	best := 0
	for i, c := range choices {
		if c <= value {
			best = i
		}
	}
	pSendMessage.Call(h, 0x014E, uintptr(best), 0) // CB_SETCURSEL
}

// enableVisualStyles gives buttons and boxes the modern Windows look by
// borrowing the common-controls v6 manifest that ships inside shell32.dll.
// If it fails the window still works, just with the old flat look.
func enableVisualStyles() {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	ctx := struct {
		size    uint32
		flags   uint32
		source  *uint16
		arch    uint16
		lang    uint16
		asmDir  *uint16
		resName uintptr
		appName *uint16
		module  uintptr
	}{flags: 0x8, source: u16ptr(root + `\System32\shell32.dll`), resName: 124}
	ctx.size = uint32(unsafe.Sizeof(ctx))
	h, _, _ := pCreateActCtx.Call(uintptr(unsafe.Pointer(&ctx)))
	if h != 0 && h != ^uintptr(0) {
		var cookie uintptr
		pActivateActCtx.Call(h, uintptr(unsafe.Pointer(&cookie)))
	}
	pInitCommonControls.Call()
}

func main() {
	runtime.LockOSThread()
	pSetDpiCtx.Call(^uintptr(3))
	enableVisualStyles()

	inst, _, _ = pGetModuleHandle.Call(0)
	arrow, _, _ = pLoadCursor.Call(0, 32512)
	audioOn.Store(true)
	clipOn.Store(true)

	mainClass, viewClass, tabsClass := u16ptr("DeskbridgeMain"), u16ptr("DeskbridgeView"), u16ptr("DeskbridgeTabs")
	appIcon, _, _ := pLoadIcon.Call(inst, 1) // the icon stamped into the .exe, if any
	wc := wndClassEx{wndProc: syscall.NewCallback(mainProc), instance: inst, cursor: arrow, icon: appIcon, iconSm: appIcon,
		background: 16 /* COLOR_BTNFACE+1 */, className: mainClass}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	vc := wndClassEx{wndProc: syscall.NewCallback(viewProc), instance: inst, cursor: arrow, className: viewClass}
	vc.size = uint32(unsafe.Sizeof(vc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&vc)))
	tc := wndClassEx{wndProc: syscall.NewCallback(tabsProc), instance: inst, cursor: arrow, className: tabsClass}
	tc.size = uint32(unsafe.Sizeof(tc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&tc)))

	const cwUseDefault = 0x80000000
	const wsExControlParent = 0x00010000
	hMain, _, _ = pCreateWindowEx.Call(wsExControlParent, uintptr(unsafe.Pointer(mainClass)), uintptr(unsafe.Pointer(u16ptr("Deskbridge"))),
		wsOverlappedWindow|wsClipChildren, cwUseDefault, cwUseDefault, 1400, 900, 0, 0, inst, 0)
	if hMain == 0 {
		return
	}
	if d, _, _ := pGetDpiForWindow.Call(hMain); d != 0 {
		dpi = int(d)
	}
	h := -int32(9 * dpi / 72)
	hFont, _, _ = pCreateFont.Call(uintptr(h), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(u16ptr("Segoe UI"))))

	buildPane()
	// It clips around its siblings so that the Take control button, which
	// sits on top of it, is not painted over.
	hView, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(viewClass)), 0, wsChild|wsVisible|wsClipSiblings, 0, 0, 10, 10, hMain, 0, inst, 0)
	hTake = control("BUTTON", "Another PC is in control  -  Take control", 0, 0, 0, 0, 300, 30, idTake)
	paneCtls = paneCtls[:len(paneCtls)-1] // not part of the pane: it comes and goes on its own
	pShowWindow.Call(hTake, 0)
	hTabs, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(tabsClass)), 0, wsChild, 0, 0, 10, 10, hMain, 0, inst, 0)
	loadSettings()
	startClipboard()
	setupFileDrop()
	pSetTimer.Call(hMain, timerFrameRate, 1000, 0) // drives the frame-rate readout
	go watchForUpdates()
	go func() { // finding decoders can take a moment; do not hold up the window
		var caps uint32
		for _, c := range []byte{proto.CodecHEVC, proto.CodecH264} {
			if mf.CanDecode(c) {
				caps |= uint32(c)
			}
		}
		videoCaps.Store(caps)
		capsKnown.Store(true)
		pPostMessage.Call(hMain, wmVideoCaps, 0, 0)
	}()

	// "deskbridge-client.exe HOST [PASSWORD]" fills the boxes and connects.
	if args := os.Args[1:]; len(args) >= 1 {
		host := args[0]
		if h, p, err := net.SplitHostPort(host); err == nil {
			host = h
			setText(hPort, p)
		}
		// A saved connection can be given by its name or by its computer.
		name := host
		for _, c := range computers {
			if strings.EqualFold(c.name, host) || strings.EqualFold(c.host, host) {
				name, host = c.name, c.host
				break
			}
		}
		setText(hName, name)
		setText(hHost, host)
		if len(args) >= 2 {
			setText(hPass, args[1])
		}
	}
	// The pane starts collapsed if there is something to connect to:
	// the tabs that were open last time; or, when started with a computer
	// to connect to (or by a version from before tabs were remembered),
	// the connection in the boxes. Otherwise the pane opens so the details
	// can be entered.
	switch {
	case len(os.Args) == 1 && launchTabsKnown:
		reopenTabs()
	case typedTarget().host != "" && getText(hPass) != "":
		connectAtLaunch()
	}
	collapsed = len(tabs) > 0
	layout()
	pShowWindow.Call(hMain, 1)
	if collapsed {
		pSetFocus.Call(hView)
	} else {
		pSetFocus.Call(hName)
	}

	// Low-level hook so Win, Alt+Tab and friends go to the remote PC while
	// the remote screen has the keyboard.
	pSetWindowsHookEx.Call(13, syscall.NewCallback(keyboardHook), inst, 0)

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		// Gives the pane Tab / Enter handling like a dialog box.
		if r, _, _ := pIsDialogMessage.Call(hMain, uintptr(unsafe.Pointer(&m))); r != 0 {
			continue
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	for _, tb := range tabs {
		if tb.l != nil {
			tb.l.close()
		}
	}
	if clip != nil {
		clip.Shutdown() // take any virtual files off the clipboard before exiting
	}
}

// ---------- settings pane ----------

func control(class, text string, style, exStyle uintptr, x, y, w, h, id int) uintptr {
	hw, _, _ := pCreateWindowEx.Call(exStyle, uintptr(unsafe.Pointer(u16ptr(class))), uintptr(unsafe.Pointer(u16ptr(text))),
		style|wsChild|wsVisible, uintptr(sc(x)), uintptr(sc(y)), uintptr(sc(w)), uintptr(sc(h)), hMain, uintptr(id), inst, 0)
	pSendMessage.Call(hw, wmSetFont, hFont, 1)
	paneCtls = append(paneCtls, hw)
	return hw
}

func buildPane() {
	const (
		esAutoHScroll, esPassword, esNumber = 0x80, 0x20, 0x2000
		bsDefPush, bsAutoCheck              = 0x1, 0x3
		cbsDropDownList                     = 0x3
		clientEdge                          = 0x200
		m, w                                = 14, paneWidth - 28
	)
	y := 14
	label := func(text string) {
		control("STATIC", text, 0, 0, m, y, w, 18, 0)
		y += 20
	}
	edit := func(style uintptr) uintptr {
		h := control("EDIT", "", style|esAutoHScroll|wsTabStop, clientEdge, m, y, w, 24, 0)
		y += 34
		return h
	}
	check := func(id int, text string) uintptr {
		h := control("BUTTON", text, bsAutoCheck|wsTabStop, 0, m, y, w, 22, id)
		y += 28
		return h
	}

	label("Connection name")
	// Type a name for a new one, or pick a saved one from the list.
	hName = control("COMBOBOX", "", 0x2|0x40|wsVScroll|wsTabStop, 0, m, y, w, 240, idName)
	y += 34
	const portW = 66
	control("STATIC", "Computer (name or IP)", 0, 0, m, y, w-portW-8, 18, 0)
	control("STATIC", "Port", 0, 0, m+w-portW, y, portW, 18, 0)
	y += 20
	hHost = control("EDIT", "", esAutoHScroll|wsTabStop, clientEdge, m, y, w-portW-8, 24, 0)
	hPort = control("EDIT", "", esNumber|esAutoHScroll|wsTabStop, clientEdge, m+w-portW, y, portW, 24, 0)
	y += 34
	label("Password")
	hPass = edit(esPassword)
	hRemember = check(idRemember, "Remember password")
	// Save keeps what is in the boxes (and is how a connection is renamed);
	// Forget removes the one named.
	// More holds exporting the list to a file and importing it.
	const third = (w - 12) / 3
	hSave = control("BUTTON", "Save", wsTabStop, 0, m, y, third, 26, idSave)
	hForget = control("BUTTON", "Forget", wsTabStop, 0, m+third+6, y, third, 26, idForget)
	hMore = control("BUTTON", "More  \u25BE", wsTabStop, 0, m+2*(third+6), y, w-2*(third+6), 26, idMore)
	pEnableWindow.Call(hForget, 0)
	y += 34
	hConnect = control("BUTTON", "Connect", bsDefPush|wsTabStop, 0, m, y, w, 32, idConnect)
	y += 50

	label("Remote display")
	hMonitor = control("COMBOBOX", "", cbsDropDownList|wsVScroll|wsTabStop, 0, m, y, w, 240, idMonitor)
	pEnableWindow.Call(hMonitor, 0)
	y += 36
	label("Remote resolution")
	hRes = control("COMBOBOX", "", cbsDropDownList|wsVScroll|wsTabStop, 0, m, y, w, 320, idResolution)
	y += 36
	// Frames per second and picture quality share a row.
	const half = (w - 8) / 2
	control("STATIC", "Frames per second", 0, 0, m, y, half, 18, 0)
	control("STATIC", "Picture quality", 0, 0, m+half+8, y, half, 18, 0)
	y += 20
	pair := func(id, x int, choices []int, suffix string) uintptr {
		h := control("COMBOBOX", "", cbsDropDownList|wsVScroll|wsTabStop, 0, x, y, half, 240, id)
		for _, c := range choices {
			pSendMessage.Call(h, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr(strconv.Itoa(c)+suffix)))) // CB_ADDSTRING
		}
		return h
	}
	hFPS = pair(idFPS, m, fpsChoices, "")
	hQuality = pair(idQuality, m+half+8, qualityChoices, " %")
	y += 36
	hAudio = check(idAudio, "Play sound from the remote PC")
	hSleep = check(idSleep, "Let the remote PC go to sleep")
	hClip = check(idClipboard, "Share copy and paste")
	hVideo = check(idVideo, "Use H.265 video for motion")
	hFast = check(idFast, "Fast screen capture")
	y += 8
	hFullscreen = control("BUTTON", "Full screen  (Ctrl+Alt+Enter)", wsTabStop, 0, m, y, w, 30, idFullscreen)
	y += 36
	hSend = control("BUTTON", "Send to remote PC  \u25BE", wsTabStop, 0, m, y, w, 30, idSend)
	pEnableWindow.Call(hSend, 0)
	y += 40
	// Shown only when a newer version is available (see update.go), so it
	// is kept out of the list of controls that follow the pane.
	hUpdate = control("BUTTON", "Update", wsTabStop, 0, m, y, w, 30, idUpdate)
	paneCtls = paneCtls[:len(paneCtls)-1]
	pShowWindow.Call(hUpdate, 0)
	y += 44
	statusTop = y
	hStatus = control("STATIC", "Not connected.", 0, 0, m, y, w, 200, 0)
	// Pinned to the bottom of the pane by layout(): this client's version
	// and the connected server's.
	hVersion = control("STATIC", "", 0, 0, m, y+204, w, versionLineH, 0)
	refreshVersionLine()

	// The collapse strip is not part of the pane: it stays visible.
	hToggle, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(u16ptr("BUTTON"))), uintptr(unsafe.Pointer(u16ptr("<"))),
		wsChild|wsVisible, 0, 0, 10, 10, hMain, idToggle, inst, 0)
	pSendMessage.Call(hToggle, wmSetFont, hFont, 1)

	setText(hPort, proto.DefaultPort)
	showResolutions()
	setCombo(hFPS, fpsChoices, 30)
	setCombo(hQuality, qualityChoices, 85)
	setChecked(hAudio, true)
	setChecked(hClip, true)
	setChecked(hVideo, true)
	setChecked(hFast, true)
	pEnableWindow.Call(hFullscreen, 0)
}

// layout places the pane, the collapse strip and the remote view.
func layout() {
	if hView == 0 {
		return
	}
	var rc rect
	pGetClientRect.Call(hMain, uintptr(unsafe.Pointer(&rc)))
	cw, ch := int(rc.right), int(rc.bottom)
	showPane := !collapsed && !fullscreen
	show := uintptr(0)
	if showPane {
		show = 8 // SW_SHOWNA
	}
	focus, _, _ := pGetFocus.Call()
	for _, c := range paneCtls {
		pShowWindow.Call(c, show)
		if !showPane && c == focus {
			pSetFocus.Call(hView)
		}
	}
	if showPane && updateOffered {
		pShowWindow.Call(hUpdate, 8)
	} else {
		if focus == hUpdate {
			pSetFocus.Call(hView)
		}
		pShowWindow.Call(hUpdate, 0)
	}
	x := 0
	if showPane {
		x = sc(paneWidth)
	}
	if fullscreen {
		pShowWindow.Call(hToggle, 0)
	} else {
		if showPane {
			setText(hToggle, "<")
		} else {
			setText(hToggle, ">")
		}
		pMoveWindow.Call(hToggle, uintptr(x), 0, uintptr(sc(toggleWidth)), uintptr(ch), 1)
		pShowWindow.Call(hToggle, 8)
		x += sc(toggleWidth)
	}
	// The strip of tabs runs along the top of the remote screen whenever a
	// connection is open. In full screen it takes no room: it floats over
	// the top of the picture as a toolbar, and only while called for (see
	// showBar).
	y, takeY := 0, sc(8)
	switch {
	case len(tabs) > 0 && !fullscreen:
		y = sc(tabsHeight)
		takeY += y
		pMoveWindow.Call(hTabs, uintptr(x), 0, uintptr(max(cw-x, 0)), uintptr(y), 1)
		pShowWindow.Call(hTabs, 8)
	case len(tabs) > 0:
		bx, bw := barSpan(cw)
		pMoveWindow.Call(hTabs, uintptr(bx), 0, uintptr(bw), uintptr(sc(tabsHeight)), 1)
		show := uintptr(0)
		if barShown {
			show = 8
		}
		pShowWindow.Call(hTabs, show)
		takeY += sc(tabsHeight) // clear of where the toolbar appears
	default:
		pShowWindow.Call(hTabs, 0)
	}
	pMoveWindow.Call(hView, uintptr(x), uintptr(y), uintptr(max(cw-x, 0)), uintptr(max(ch-y, 0)), 1)
	// The Take control button sits at the top of the remote screen, in
	// the middle, whenever another PC has control of it.
	bw := min(sc(300), max(cw-x-sc(16), sc(120)))
	pMoveWindow.Call(hTake, uintptr(x+max(cw-x-bw, 0)/2), uintptr(takeY), uintptr(bw), uintptr(sc(30)), 1)
	showTakeControl()

	// The version line sits at the bottom of the pane; the status text
	// takes whatever height is left above it.
	const m, w = 14, paneWidth - 28
	versionY := max(ch-sc(versionLineH+10), sc(statusTop+24))
	pMoveWindow.Call(hVersion, uintptr(sc(m)), uintptr(versionY), uintptr(sc(w)), uintptr(sc(versionLineH)), 1)
	pMoveWindow.Call(hStatus, uintptr(sc(m)), uintptr(sc(statusTop)), uintptr(sc(w)), uintptr(max(versionY-sc(statusTop)-sc(8), sc(16))), 1)
}

const versionLineH = 18

// refreshVersionLine fills in the line at the bottom of the pane.
func refreshVersionLine() {
	server := "not connected"
	if l := cur.Load(); l != nil {
		server = "version not reported" // servers older than 1.1.0 do not say
		if v := l.version.Load(); v != nil {
			server = *v
		}
	}
	line := "Client " + proto.Version + "   |   Server " + server
	if cur.Load() != nil && measuredFPS >= 0 {
		// What is actually arriving, which can be well under the setting.
		line += "   |   " + strconv.Itoa(measuredFPS) + " fps"
	}
	setText(hVersion, line)
}

// framesShown counts frames as they arrive; once a second the UI thread
// turns the count into the rate shown at the bottom of the pane.
var (
	framesShown atomic.Int32
	measuredFPS = -1 // UI thread only; -1 until the first measurement
)

func setStatus(s string) { setText(hStatus, s) }

// settingsPayload is a connection's settings as sent to its server. A
// connection that is not in front is asked for much less: a couple of
// frames a second to keep its picture current, no sound and no video.
func settingsPayload(o options, front bool) []byte {
	b := []byte{byte(o.fps), byte(o.quality), 0, 0, 0, 0, 0, 0}
	if !front {
		b[0] = backgroundFPS
	}
	if o.audio && front {
		b[2] = 1
	}
	if o.sleep {
		b[3] = 1
	}
	if o.clip {
		b[4] = 1
	}
	if o.video && front {
		b[5] = 1
	}
	b[6] = byte(videoCaps.Load())
	if o.fast {
		b[7] = 1
	}
	return b
}

const backgroundFPS = 2

// applySettings runs when one of the pane's settings is changed. The
// change belongs to the tab in front (and is sent to its server) and to
// that connection's saved entry; with no tab open, to the saved connection
// named in the boxes.
func applySettings() {
	o := readOptions()
	audioOn.Store(o.audio)
	clipOn.Store(o.clip)
	if active != nil {
		active.opts = o
		if active.l != nil {
			sendSettings(active.l)
		}
	}
	if i := currentProfile(); i >= 0 {
		computers[i].opts, computers[i].hasOpts = o, true
	}
	saveSettings()
}

// resendSettings sends every connected server its settings again.
func resendSettings() {
	for _, tb := range tabs {
		if tb.l != nil {
			sendSettings(tb.l)
		}
	}
}

// ---------- saved settings ----------

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Deskbridge", "client.txt")
}

type dataBlob struct {
	size uint32
	data *byte
}

// dpapi encrypts or decrypts with a key tied to this Windows account, so a
// remembered password is not left on disk in the clear.
func dpapi(proc *syscall.LazyProc, in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	src := dataBlob{uint32(len(in)), &in[0]}
	var dst dataBlob
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&src)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&dst))); r == 0 || dst.data == nil {
		return nil
	}
	out := append([]byte(nil), unsafe.Slice(dst.data, dst.size)...)
	pLocalFree.Call(uintptr(unsafe.Pointer(dst.data)))
	return out
}

func saveSettings() {
	path := settingsPath()
	if path == "" {
		return
	}
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	lines := []string{
		"name=" + cleanName(getText(hName)),
		"host=" + getText(hHost),
		"port=" + getText(hPort),
		"fps=" + strconv.Itoa(comboValue(hFPS, fpsChoices)),
		"quality=" + strconv.Itoa(comboValue(hQuality, qualityChoices)),
		"audio=" + b(checked(hAudio)),
		"sleep=" + b(checked(hSleep)),
		"clipboard=" + b(checked(hClip)),
		"video=" + b(checked(hVideo)),
		"fastcapture=" + b(checked(hFast)),
		"remember=" + b(checked(hRemember)),
	}
	// The tabs to open again next time, and the one in front.
	var open []string
	for _, tb := range tabs {
		if !tb.stopped() {
			open = append(open, tb.target.name)
		}
	}
	front := ""
	if active != nil {
		front = active.target.name
	}
	lines = append(lines, "open="+strings.Join(open, "|"), "front="+front)
	if checked(hRemember) {
		if enc := dpapi(pCryptProtectData, []byte(getText(hPass))); enc != nil {
			lines = append(lines, "password="+hex.EncodeToString(enc))
		}
	}
	lines = append(lines, computerLines()...)
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0600)
}

func loadSettings() {
	var lastPassword []byte
	oldDisplay := noPreference // from before each connection kept its own
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(v)
		switch k {
		case "name":
			setText(hName, v)
		case "host":
			setText(hHost, v)
		case "port":
			if n > 0 && n < 65536 {
				setText(hPort, v)
			}
		case "fps":
			setCombo(hFPS, fpsChoices, n)
		case "quality":
			setCombo(hQuality, qualityChoices, n)
		case "audio":
			setChecked(hAudio, v == "1")
		case "sleep":
			setChecked(hSleep, v == "1")
		case "clipboard":
			setChecked(hClip, v == "1")
		case "video":
			setChecked(hVideo, v == "1")
		case "fastcapture":
			setChecked(hFast, v == "1")
		case "display":
			if n >= -1 && n < 16 && v != "" {
				oldDisplay = n
			}
		case "remember":
			setChecked(hRemember, v == "1")
		case "open":
			launchTabsKnown = true
			if v != "" {
				launchTabs = strings.Split(v, "|")
			}
		case "front":
			launchFront = v
		case "password":
			if enc, err := hex.DecodeString(v); err == nil {
				lastPassword = enc
				if pw := dpapi(pCryptUnprotectData, enc); pw != nil {
					setText(hPass, string(pw))
				}
			}
		case "computer": // saved before connections had names
			before := len(computers)
			parseComputer(v)
			if len(computers) > before {
				computers[before].display = oldDisplay
			}
		case "connection":
			parseConnection(v)
		}
	}
	// Settings from before there was a list held one computer: start the
	// list with it.
	if host := getText(hHost); host != "" && len(computers) == 0 {
		computers = append(computers, computer{name: host, host: host, port: getText(hPort), password: lastPassword, display: oldDisplay})
	}
	if getText(hName) == "" {
		setText(hName, getText(hHost))
	}
	fillComputers()
	if i := findComputer(cleanName(getText(hName))); i >= 0 {
		pickedName = computers[i].name
		loadProfile(i)
	}
	audioOn.Store(checked(hAudio))
	clipOn.Store(checked(hClip))
}

// ---------- connecting ----------

func openPane() {
	if collapsed {
		collapsed = false
		layout()
	}
}

// ---------- network ----------

func queue(t byte, payload []byte) {
	l := cur.Load()
	if l == nil {
		return
	}
	select {
	case l.out <- [2][]byte{{t}, payload}:
	default: // connection stalled; drop rather than freeze the UI
	}
}

func sender(l *link) {
	for {
		var m [2][]byte
		select {
		case m = <-l.out: // input always goes ahead of clipboard data
		default:
			select {
			case m = <-l.out:
			case m = <-l.bulk:
			case <-l.done:
				return
			}
		}
		if l.c.Send(m[0][0], true, m[1]) != nil {
			return
		}
	}
}

// ---------- video for motion ----------

// videoNote explains, when it matters, why H.265 is not what is being used.
func videoNote() string {
	if !checked(hVideo) || !capsKnown.Load() {
		return ""
	}
	caps := byte(videoCaps.Load())
	switch {
	case caps&proto.CodecHEVC != 0:
		return ""
	case caps&proto.CodecH264 != 0:
		return "\r\n\r\nThis PC has no H.265 decoder, so motion uses H.264 instead. For H.265, install \"HEVC Video Extensions\" from the Microsoft Store and restart Deskbridge."
	}
	return "\r\n\r\nThis PC has no video decoder Deskbridge can use, so motion is sent the standard way."
}

// gpuBroken is set if decoding on the graphics card failed, so the rest of
// this run uses the software decoder.
var gpuBroken atomic.Bool

// videoSink decodes the video the server sends during motion and draws it
// into the framebuffer. Used only by the receiver goroutine.
type videoSink struct {
	dec       *mf.Decoder
	codec     byte
	w, h      int
	failed    bool   // ignore video until the server starts a new stream
	announced bool   // said which decoder is in use
	notice    string // what the server said about its encoder
	capture   string // what the server said about its screen capture
	decoded   string // how video is being decoded here, once known
	l         *link  // the connection whose picture this draws into
	host      string
	restart   func() // asks the server for a new stream
	// The changed areas that came with the last few frames. Only those are
	// redrawn from video, so the rest of the screen stays pixel-perfect. A
	// few frames' worth are kept because a decoder may hand pictures back a
	// frame or two late.
	recent [3][]proto.Rect
}

// report is the pane's summary of how the picture is being produced.
func (v *videoSink) report() string {
	s := "Connected to " + v.host + "."
	if v.capture != "" {
		s += "\r\n\r\n" + v.capture
	}
	if v.decoded != "" {
		s += "\r\n\r\n" + v.decoded
	}
	if !v.l.inControl.Load() {
		return s + "\r\n\r\n" + watchingNote(v.l)
	}
	return s + "\r\n\r\nClick the remote screen to type there; click in this pane to type here."
}

func (v *videoSink) close() {
	if v.dec != nil {
		v.dec.Close()
		v.dec = nil
	}
}

// fail gives up on video for this connection and has the option unticked.
func (v *videoSink) fail(err error) {
	v.failed = true
	v.close()
	s := "Video for motion was switched off because decoding failed (" + err.Error() + "). The picture continues the standard way."
	v.l.status.Store(&s)
	pPostMessage.Call(hMain, wmVideoFailed, v.l.id, 0)
}

func (v *videoSink) handle(p []byte) {
	if len(p) < 8 {
		return
	}
	flags, codec := p[0], p[1]
	w, h := int(binary.LittleEndian.Uint16(p[2:])), int(binary.LittleEndian.Uint16(p[4:]))
	n := int(binary.LittleEndian.Uint16(p[6:]))
	if len(p) < 8+8*n+1 {
		return
	}
	if n > 0 {
		areas := make([]proto.Rect, n)
		for i := range areas {
			q := p[8+8*i:]
			areas[i] = proto.Rect{X: int(binary.LittleEndian.Uint16(q)), Y: int(binary.LittleEndian.Uint16(q[2:])),
				W: int(binary.LittleEndian.Uint16(q[4:])), H: int(binary.LittleEndian.Uint16(q[6:]))}
		}
		v.recent[0], v.recent[1], v.recent[2] = areas, v.recent[0], v.recent[1]
	}
	frame := p[8+8*n:]
	if flags&2 != 0 {
		v.failed = false // the server started a fresh stream: try again
	}
	if v.failed || w < 2 || h < 2 {
		return
	}
	if v.dec == nil || flags&2 != 0 || codec != v.codec || w != v.w || h != v.h {
		v.close()
		dec, err := mf.NewDecoder(codec, w, h, !gpuBroken.Load())
		if err != nil {
			v.fail(err)
			return
		}
		v.dec, v.codec, v.w, v.h, v.announced = dec, codec, w, h, false
	}
	var areas []proto.Rect
	for _, a := range v.recent {
		areas = append(areas, a...)
	}
	shown := false
	err := v.dec.Decode(frame, func(y, uv []byte, stride int) {
		shown = true
		l := v.l
		l.fbMu.RLock()
		if l.fb != nil && l.fbW >= w && l.fbH >= h {
			proto.NV12RectsToBGRA(l.fb, l.fbW, y, uv, stride, w, h, areas)
		}
		l.fbMu.RUnlock()
	})
	if err != nil {
		if v.dec.AskedGPU() && v.restart != nil {
			// The graphics card's decoder let us down. Fall back to the
			// software decoder; that needs a stream that starts afresh.
			gpuBroken.Store(true)
			v.failed = true
			v.close()
			v.restart()
			return
		}
		v.fail(err)
		return
	}
	if shown && !v.announced {
		v.announced = true
		if v.notice == "" {
			v.notice = "Motion is being sent as " + mf.CodecName(codec) + " video."
		}
		where := "in software (not on the graphics card)"
		if v.dec.OnGPU() {
			where = "on the graphics card"
		}
		v.decoded = v.notice + " Decoded here " + where + " by: " + v.dec.Name + "."
		v.l.postStatus(v.report())
	}
}

// ---------- clipboard ----------

var clip *winclip.Sync

func postStatus(s string) {
	statusText.Store(&s)
	pPostMessage.Call(hMain, wmStatus, 0, 0)
}

func startClipboard() {
	// Earlier versions kept pasted files here; nothing does any more.
	os.RemoveAll(filepath.Join(os.TempDir(), "Deskbridge", "Clipboard"))
	// Copy and paste is shared with the connection in front, and only
	// while this PC is the one in control of it.
	sharing := func() bool {
		l := cur.Load()
		return clipOn.Load() && (l == nil || l.inControl.Load())
	}
	c := &winclip.Sync{Enabled: sharing, Status: postStatus, Shown: dropShown}
	if c.Start() {
		clip = c
	}
}

func receiver(l *link) {
	defer pPostMessage.Call(hMain, wmDisconnected, l.id, 0)
	type job struct {
		enc  byte
		data []byte
		r    proto.Rect
	}
	jobs := make(chan job, 256)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			for j := range jobs {
				l.fbMu.RLock()
				if l.fb != nil {
					proto.DecodeRect(j.enc, j.data, j.r, l.fb, l.fbW, l.fbH)
				}
				l.fbMu.RUnlock()
				wg.Done()
			}
		}()
	}
	defer close(jobs)
	var audio player
	defer audio.close()
	video := videoSink{l: l, host: l.name, restart: func() {
		select {
		case l.bulk <- [2][]byte{{proto.MsgVideoRestart}, nil}:
		case <-l.done:
		}
	}}
	defer video.close()
	peer := l.peer
	if peer != nil {
		defer peer.Close()
	}

	for {
		t, p, err := l.c.Read()
		if err != nil {
			return
		}
		l.heard.Store(time.Now().UnixNano())
		switch t {
		case proto.MsgSize:
			if len(p) < 8 {
				return
			}
			w, h := int(binary.LittleEndian.Uint32(p)), int(binary.LittleEndian.Uint32(p[4:]))
			if w <= 0 || h <= 0 || w > 65535 || h > 65535 {
				return
			}
			wg.Wait()
			l.fbMu.Lock()
			l.fb, l.fbW, l.fbH = make([]byte, w*h*4), w, h
			l.fbMu.Unlock()
			pPostMessage.Call(hMain, wmRemoteSize, l.id, 0)
		case proto.MsgRect:
			if len(p) < 9 {
				return
			}
			r := proto.Rect{
				X: int(binary.LittleEndian.Uint16(p[0:])), Y: int(binary.LittleEndian.Uint16(p[2:])),
				W: int(binary.LittleEndian.Uint16(p[4:])), H: int(binary.LittleEndian.Uint16(p[6:])),
			}
			wg.Add(1)
			jobs <- job{p[8], p[9:], r}
		case proto.MsgPong:
			l.pong.Store(true)
		case proto.MsgControl:
			if len(p) >= 1 && len(p) <= 200 {
				name := string(p[1:])
				l.controller.Store(&name)
				l.inControl.Store(p[0] == 1)
				pPostMessage.Call(hMain, wmControl, l.id, 0)
			}
		case proto.MsgPointer:
			if len(p) >= 5 {
				l.pointer.Store(uint64(binary.LittleEndian.Uint16(p)) | uint64(binary.LittleEndian.Uint16(p[2:]))<<16 | uint64(p[4]&1)<<32)
				if cur.Load() == l && !l.inControl.Load() {
					pPostMessage.Call(hView, wmRepaint, 0, 0)
				}
			}
		case proto.MsgMonitors:
			if d := parseDisplays(p); d != nil {
				l.displays.Store(d)
				pPostMessage.Call(hMain, wmMonitors, l.id, 0)
			}
		case proto.MsgModes:
			if m := parseModes(p); m != nil {
				l.modes.Store(m)
				pPostMessage.Call(hMain, wmModes, l.id, 0)
			}
		case proto.MsgUpdateResult:
			select {
			case l.updateResult <- append([]byte(nil), p...):
			default:
			}
		case proto.MsgNotice:
			if len(p) < 2 || len(p) > 600 {
				continue
			}
			switch p[0] {
			case proto.NoticeVideo:
				video.notice = string(p[1:])
			case proto.NoticeVersion:
				v := string(p[1:])
				l.version.Store(&v)
				pPostMessage.Call(hMain, wmUpdateInfo, 0, 0)
			case proto.NoticeReplaced:
				l.replaced.Store(true)
			case proto.NoticeGeneral:
				l.postStatus(string(p[1:]))
			case proto.NoticeCapture:
				video.capture = string(p[1:])
				l.postStatus(video.report())
			}
		case proto.MsgVideo:
			wg.Wait() // anything drawn before this frame must land first
			video.handle(p)
		case proto.MsgFrameEnd:
			wg.Wait()
			if cur.Load() == l { // only the connection in front is on show
				framesShown.Add(1)
				pPostMessage.Call(hView, wmRepaint, 0, 0)
			}
			select { // must not be dropped: the server waits for it
			case l.out <- [2][]byte{{proto.MsgAck}, nil}:
			case <-l.done:
				return
			}
		case proto.MsgCursor:
			if len(p) >= 2 {
				if h, _, _ := pLoadCursor.Call(0, uintptr(binary.LittleEndian.Uint16(p))); h != 0 {
					l.cursor.Store(h)
					if cur.Load() == l {
						pPostMessage.Call(hView, wmCursor, 0, 0)
					}
				}
			}
		case proto.MsgAudioFmt:
			if len(p) >= 4 {
				audio.open(binary.LittleEndian.Uint32(p))
			}
		case proto.MsgAudio:
			if audioOn.Load() && cur.Load() == l { // one PC's sound at a time
				audio.play(p)
			}
		default:
			if peer != nil && proto.IsClipMsg(t) {
				peer.Remote(t, p)
			}
		}
	}
}

// ---------- main window ----------

func mainProc(h, message, wParam, lParam uintptr) uintptr {
	if updateMessage(message, wParam) {
		return 0
	}
	switch message {
	case wmMonitors: // wParam: the connection whose list of displays arrived
		if tb := tabOfLink(wParam); tb != nil {
			requestDisplay(tb)
			if tb == active {
				showDisplays()
			}
		}
		return 0
	case wmModes: // wParam: the connection whose resolutions arrived
		if tb := tabOfLink(wParam); tb != nil {
			modesArrived(tb)
		}
		return 0
	case wmNewConnection:
		newConnection()
		refreshConnectButton()
		return 0
	case wmNameChanged:
		refreshConnectButton()
		return 0
	case wmSwitchTab:
		stepTab(wParam == 1)
		return 0
	case wmDropShown:
		dropConfirmed(uint32(wParam), lParam == 1)
		return 0
	case wmControl: // wParam: the connection whose state of control changed
		if tb := tabOfLink(wParam); tb != nil {
			controlChanged(tb)
		}
		return 0
	}
	switch message {
	case wmSize:
		layout()
		return 0
	case wmCommand:
		id, code := int(wParam&0xFFFF), int(wParam>>16&0xFFFF)
		switch {
		case id == idConnect && code == 0:
			connectClicked()
		case id == idName && code == 1: // CBN_SELCHANGE: a saved connection was picked
			computerPicked()
			// The list puts the name in the box after telling us; relabel
			// the Connect button once it has.
			pPostMessage.Call(hMain, wmNameChanged, 0, 0)
		case id == idName && code == 5: // CBN_EDITCHANGE: typing
			forgettable := uintptr(0)
			if findComputer(cleanName(getText(hName))) >= 0 {
				forgettable = 1
			}
			pEnableWindow.Call(hForget, forgettable)
			refreshConnectButton()
		case id == idResolution && code == 1: // CBN_SELCHANGE
			resolutionPicked()
		case id == idForget && code == 0:
			forgetComputer()
		case id == idSave && code == 0:
			saveConnection()
		case id == idMore && code == 0:
			moreMenu()
		case id == idTake && code == 0:
			takeControlClicked()
		case (id == idFPS || id == idQuality) && code == 1, // CBN_SELCHANGE
			(id == idAudio || id == idSleep || id == idRemember || id == idClipboard || id == idVideo || id == idFast) && code == 0:
			applySettings()
		case id == idFullscreen && code == 0:
			if cur.Load() != nil {
				toggleFullscreen()
			}
		case id == idUpdate && code == 0:
			startUpdate()
		case id == idMonitor && code == 1: // CBN_SELCHANGE
			displayPicked()
		case id == idSend && code == 0:
			sendMenu()
		case id == idToggle && code == 0:
			collapsed = !collapsed
			layout()
			if cur.Load() != nil {
				pSetFocus.Call(hView)
			}
		}
		return 0
	case 0x00A0, 0x0231: // WM_NCMOUSEMOVE, WM_ENTERSIZEMOVE
		if !localKeys {
			localKeys = true
			releaseKeys()
		}
	case wmActivate:
		if wParam&0xFFFF == 0 {
			lastFocus, _, _ = pGetFocus.Call()
			releaseKeys()
			clear(keysDown) // their releases may go unseen while another window is in front
		}
	case wmSetFocus:
		// A plain window does not hand focus back to a child by itself.
		target := lastFocus
		if v, _, _ := pIsWindowVisible.Call(target); target == 0 || target == h || v == 0 {
			target = hName
			if cur.Load() != nil || collapsed {
				target = hView
			}
		}
		pSetFocus.Call(target)
		return 0
	case wmFullscreen:
		if cur.Load() != nil {
			toggleFullscreen()
		}
		return 0
	case wmRemoteSize:
		if l := cur.Load(); l != nil && l.id == wParam {
			refreshTitle()
			pInvalidateRect.Call(hView, 0, 0)
		}
		return 0
	case wmConnectDone:
		onConnectDone()
		return 0
	case wmVideoCaps:
		resendSettings() // tell the servers what we can decode
		if cur.Load() != nil {
			if note := videoNote(); note != "" {
				setStatus(strings.TrimSpace(note))
			}
		}
		return 0
	case wmVideoFailed: // wParam: the connection whose decoder gave up
		if tb := tabOfLink(wParam); tb != nil {
			if tb.opts.video {
				tb.opts.video = false
				if tb == active {
					setChecked(hVideo, false)
				}
				if i := findComputer(tb.target.name); i >= 0 && computers[i].hasOpts {
					computers[i].opts.video = false
				}
				sendSettings(tb.l)
				saveSettings()
			}
			if s := tb.l.status.Swap(nil); s != nil {
				setTabStatus(tb, *s)
			}
		}
		return 0
	case wmStatus:
		if wParam == 0 { // not about one connection: clipboard sharing, say
			if s := statusText.Swap(nil); s != nil && cur.Load() != nil {
				setStatus(*s)
			}
			return 0
		}
		if tb := tabOfLink(wParam); tb != nil {
			if s := tb.l.status.Swap(nil); s != nil {
				text := *s
				if strings.HasPrefix(text, "Connected to ") {
					text += videoNote() // e.g. that this PC lacks an H.265 decoder
				}
				setTabStatus(tb, text)
			}
		}
		return 0
	case wmDisconnected:
		onDisconnected(wParam)
		return 0
	case 0x0113: // WM_TIMER
		if wParam >= timerReconnectBase {
			reconnectTimer(wParam)
			return 0
		}
		if wParam == timerDropHint || wParam == timerDropPaste {
			dropTimer(wParam)
			return 0
		}
		if wParam == timerBarShow || wParam == timerBarHide {
			barTimer(wParam)
			return 0
		}
		// Otherwise the once-a-second tick behind the frame-rate readout.
		n := int(framesShown.Swap(0))
		if cur.Load() == nil {
			n = -1
		}
		if n != measuredFPS {
			measuredFPS = n
			refreshVersionLine()
		}
		return 0
	case wmDestroy:
		saveSettings()
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(h, message, wParam, lParam)
	return r
}

func toggleFullscreen() {
	showBar(false) // the toolbar belongs to the mode being left
	h := hMain
	const gwlStyle = ^uintptr(15) // -16
	const swpFrameChanged, swpNoZOrder = 0x0020, 0x0004
	if !fullscreen {
		mon, _, _ := pMonitorFromWindow.Call(h, 2)
		mi := monitorInfo{size: 40}
		if r, _, _ := pGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return
		}
		savedStyle, _, _ = pGetWindowLongPtr.Call(h, gwlStyle)
		if savedStyle&wsMaximize != 0 {
			pShowWindow.Call(h, 9) // restore first so the normal size is what gets saved
		}
		pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&savedRect)))
		fullscreen = true
		pSetWindowLongPtr.Call(h, gwlStyle, wsPopup|wsVisible|wsClipChildren)
		m := mi.monitor
		pSetWindowPos.Call(h, 0, uintptr(m.left), uintptr(m.top), uintptr(m.right-m.left), uintptr(m.bottom-m.top), swpFrameChanged|swpNoZOrder)
	} else {
		fullscreen = false
		pSetWindowLongPtr.Call(h, gwlStyle, (savedStyle|wsVisible)&^wsMaximize)
		r := savedRect
		pSetWindowPos.Call(h, 0, uintptr(r.left), uintptr(r.top), uintptr(r.right-r.left), uintptr(r.bottom-r.top), swpFrameChanged|swpNoZOrder)
		if savedStyle&wsMaximize != 0 {
			pShowWindow.Call(h, 3)
		}
	}
	layout()
	pSetFocus.Call(hView)
}

// ---------- remote view ----------

// destRect is where the remote screen is drawn inside the view: as large as
// fits, same aspect ratio, centred.
func destRect() (dx, dy, dw, dh, rw, rh int) {
	var rc rect
	pGetClientRect.Call(hView, uintptr(unsafe.Pointer(&rc)))
	cw, ch := int(rc.right), int(rc.bottom)
	if l := cur.Load(); l != nil {
		l.fbMu.RLock()
		rw, rh = l.fbW, l.fbH
		l.fbMu.RUnlock()
	}
	if rw == 0 || rh == 0 || cw == 0 || ch == 0 {
		return 0, 0, 0, 0, rw, rh
	}
	dw, dh = cw, cw*rh/rw
	if dh > ch {
		dw, dh = ch*rw/rh, ch
	}
	return (cw - dw) / 2, (ch - dh) / 2, dw, dh, rw, rh
}

func remotePoint(lParam uintptr) (x, y int, ok bool) {
	return remoteAt(int(int16(lParam)), int(int16(lParam>>16)))
}

// remoteAt turns a position in the view into one on the remote screen.
func remoteAt(cx, cy int) (x, y int, ok bool) {
	dx, dy, dw, dh, rw, rh := destRect()
	if dw == 0 || dh == 0 {
		return 0, 0, false
	}
	x = min(max((cx-dx)*rw/dw, 0), rw-1)
	y = min(max((cy-dy)*rh/dh, 0), rh-1)
	return x, y, true
}

func xy(x, y int) []byte {
	b := binary.LittleEndian.AppendUint16(nil, uint16(x))
	return binary.LittleEndian.AppendUint16(b, uint16(y))
}

func viewProc(h, message, wParam, lParam uintptr) uintptr {
	button := func(b byte, down bool) uintptr {
		if down {
			pSetFocus.Call(h)
		}
		if x, y, ok := remotePoint(lParam); ok {
			d := byte(0)
			if down {
				d = 1
				pSetCapture.Call(h)
			} else {
				pReleaseCapture.Call()
			}
			queue(proto.MsgMouseBtn, append([]byte{b, d}, xy(x, y)...))
		}
		return 0
	}
	switch message {
	case wmPaint:
		paint(h)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmKillFocus:
		releaseKeys()
		return 0
	case wmMouseMove:
		localKeys = false
		if fullscreen {
			barPointer(int(int16(lParam)), int(int16(lParam>>16)))
		}
		if x, y, ok := remotePoint(lParam); ok {
			queue(proto.MsgMouseMove, xy(x, y))
		}
		return 0
	case wmLButtonDown:
		return button(0, true)
	case wmLButtonUp:
		return button(0, false)
	case wmRButtonDown:
		return button(1, true)
	case wmRButtonUp:
		return button(1, false)
	case wmMButtonDown:
		return button(2, true)
	case wmMButtonUp:
		return button(2, false)
	case wmXButtonDown, wmXButtonUp:
		b := byte(3)
		if (wParam>>16)&0xFFFF == 2 {
			b = 4
		}
		button(b, message == wmXButtonDown)
		return 1
	case wmMouseWheel, wmMouseHWheel:
		p := binary.LittleEndian.AppendUint16(nil, uint16(wParam>>16))
		if message == wmMouseHWheel {
			p = append(p, 1)
		} else {
			p = append(p, 0)
		}
		queue(proto.MsgWheel, p)
		return 0
	case wmSetCursor:
		if l := cur.Load(); lParam&0xFFFF == 1 && l != nil { // HTCLIENT
			pSetCursor.Call(l.cursor.Load())
			return 1
		}
	case wmCursor:
		var pt [2]int32
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		pScreenToClient.Call(h, uintptr(unsafe.Pointer(&pt)))
		var rc rect
		pGetClientRect.Call(h, uintptr(unsafe.Pointer(&rc)))
		if fg, _, _ := pGetForegroundWindow.Call(); fg == hMain && pt[0] >= 0 && pt[1] >= 0 && pt[0] < rc.right && pt[1] < rc.bottom {
			if l := cur.Load(); l != nil {
				pSetCursor.Call(l.cursor.Load())
			}
		}
		return 0
	case wmRepaint:
		pInvalidateRect.Call(h, 0, 0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(h, message, wParam, lParam)
	return r
}

func paint(h uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(h, uintptr(unsafe.Pointer(&ps)))
	defer pEndPaint.Call(h, uintptr(unsafe.Pointer(&ps)))
	var rc rect
	pGetClientRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	cw, ch := int(rc.right), int(rc.bottom)
	dx, dy, dw, dh, _, _ := destRect()
	const blackness = 0x42
	if dw == 0 || dh == 0 {
		pPatBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), blackness)
		text := "Not connected"
		if cur.Load() != nil {
			text = "Waiting for the remote screen..."
		} else if active != nil && active.viewNote != "" {
			text = active.viewNote
		}
		old, _, _ := pSelectObject.Call(hdc, hFont)
		pSetBkMode.Call(hdc, 1)
		pSetTextColor.Call(hdc, 0x00909090)
		pDrawText.Call(hdc, uintptr(unsafe.Pointer(u16ptr(text))), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), 0x25) // centred, single line
		pSelectObject.Call(hdc, old)
		return
	}
	if dx > 0 {
		pPatBlt.Call(hdc, 0, 0, uintptr(dx), uintptr(ch), blackness)
		pPatBlt.Call(hdc, uintptr(dx+dw), 0, uintptr(cw-dx-dw), uintptr(ch), blackness)
	}
	if dy > 0 {
		pPatBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(dy), blackness)
		pPatBlt.Call(hdc, 0, uintptr(dy+dh), uintptr(cw), uintptr(ch-dy-dh), blackness)
	}
	l := cur.Load()
	if l == nil {
		return
	}
	l.fbMu.Lock()
	defer l.fbMu.Unlock()
	fb, fbW, fbH := l.fb, l.fbW, l.fbH
	if fb == nil {
		return
	}
	bi := bitmapInfoHeader{Size: 40, Width: int32(fbW), Height: -int32(fbH), Planes: 1, BitCount: 32}
	if dw == fbW && dh == fbH {
		pSetStretchBltMode.Call(hdc, 3) // COLORONCOLOR
	} else {
		pSetStretchBltMode.Call(hdc, 4) // HALFTONE: smooth scaling
		pSetBrushOrgEx.Call(hdc, 0, 0, 0)
	}
	pStretchDIBits.Call(hdc, uintptr(dx), uintptr(dy), uintptr(dw), uintptr(dh),
		0, 0, uintptr(fbW), uintptr(fbH),
		uintptr(unsafe.Pointer(&fb[0])), uintptr(unsafe.Pointer(&bi)), 0, 0x00CC0020)
	// While another PC is in control, the picture alone does not show
	// where its pointer is: draw it.
	if at := l.pointer.Load(); !l.inControl.Load() && at>>32&1 != 0 {
		px, py := int(at&0xFFFF), int(at>>16&0xFFFF)
		pDrawIconEx.Call(hdc, uintptr(dx+px*dw/fbW), uintptr(dy+py*dh/fbH), l.cursor.Load(), 0, 0, 0, 0, 3) // DI_NORMAL
	}
}

// ---------- keyboard ----------

func sendKey(k kbdllHook, up bool) {
	// Windows reports a few keys to keyboard hooks with scan-code details
	// that do not replay correctly on the other side:
	//  - Right Shift and Num Lock arrive flagged "extended" although their
	//    real scan codes are not; replayed that way they are ignored.
	//  - Pause shares Num Lock's scan code, so it is sent by key code.
	scan, extended := uint16(k.scan), k.flags&0x01 != 0
	switch k.vk {
	case 0xA1, 0x90: // VK_RSHIFT, VK_NUMLOCK
		extended = false
	case 0x13: // VK_PAUSE
		scan, extended = 0, false
	}
	p := binary.LittleEndian.AppendUint16(nil, uint16(k.vk))
	p = binary.LittleEndian.AppendUint16(p, scan)
	var f byte
	if extended {
		f |= 1
	}
	if up {
		f |= 2
	}
	queue(proto.MsgKey, append(p, f))
}

// releaseKeys lifts every key the remote PC thinks is held, so nothing gets
// stuck down when the remote view loses the keyboard.
func releaseKeys() {
	for id, k := range pressed {
		sendKey(k, true)
		delete(pressed, id)
	}
}

// keyboardHook forwards keys only while connected, this window is in front,
// the remote view (not the settings pane) has the keyboard, and the pointer
// is not on the window frame.
func keyboardHook(nCode, wParam, lParam uintptr) uintptr {
	if int32(nCode) != 0 {
		r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	k := *(*kbdllHook)(unsafe.Pointer(lParam))
	up := k.flags&0x80 != 0
	id := k.vk | (k.flags&1)<<16
	if up {
		delete(keysDown, k.vk)
	} else {
		keysDown[k.vk] = true
	}

	// A key belongs to whichever PC saw it go down. If this PC saw the
	// press (say Shift held while dragging the window), it must also see
	// the release, even if the remote view has the keyboard by then;
	// otherwise the key stays stuck down here.
	if localDown[id] && !up {
		// Still physically held (auto-repeat), or a stale entry?
		if st, _, _ := pGetAsyncKeyState.Call(uintptr(k.vk)); st&0x8000 == 0 {
			delete(localDown, id)
		}
	}
	forward := false
	if hView != 0 && cur.Load() != nil && !localKeys && !localDown[id] {
		fg, _, _ := pGetForegroundWindow.Call()
		focus, _, _ := pGetFocus.Call()
		forward = fg == hMain && focus == hView
	}
	if _, remote := pressed[id]; up && !remote && !swallowing[k.vk] {
		forward = false // a release we never sent the press for
	}
	if !forward {
		if up {
			delete(localDown, id)
		} else {
			localDown[id] = true
		}
		r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}

	const vkReturn, vkEnd, vkLCtrl, vkRCtrl, vkLAlt, vkRAlt = 0x0D, 0x23, 0xA2, 0xA3, 0xA4, 0xA5
	// Whether any of these keys is down. Switching tabs lifts the keys
	// held on the PC being left, so this goes by the keyboard itself: then
	// Ctrl+Alt can stay down while stepping through several tabs.
	held := func(vks ...uint32) bool {
		for _, v := range vks {
			if keysDown[v] {
				return true
			}
		}
		return false
	}
	// Our own shortcuts, all with Ctrl+Alt held. The key's press and
	// release are both kept from the remote PC.
	const vkPageUp, vkPageDown = 0x21, 0x22
	if k.vk == vkEnd || k.vk == vkReturn || k.vk == vkPageUp || k.vk == vkPageDown {
		if !up && held(vkLCtrl, vkRCtrl) && held(vkLAlt, vkRAlt) {
			if !swallowing[k.vk] {
				swallowing[k.vk] = true
				switch k.vk {
				case vkEnd:
					// Stands in for Ctrl+Alt+Del, which Windows always
					// keeps for this PC.
					queue(proto.MsgAction, []byte{proto.ActionCtrlAltDel})
				case vkReturn:
					pPostMessage.Call(hMain, wmFullscreen, 0, 0)
				case vkPageDown:
					pPostMessage.Call(hMain, wmSwitchTab, 1, 0) // the next tab
				case vkPageUp:
					pPostMessage.Call(hMain, wmSwitchTab, 0, 0) // the previous tab
				}
			}
			return 1
		}
		if up && swallowing[k.vk] {
			delete(swallowing, k.vk)
			return 1
		}
	}
	if up {
		delete(pressed, id)
	} else {
		pressed[id] = k
	}
	sendKey(k, up)
	return 1
}

// ---------- audio ----------

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type waveHdr struct {
	data          *byte
	bufferLength  uint32
	bytesRecorded uint32
	user          uintptr
	flags         uint32
	loops         uint32
	next          uintptr
	reserved      uintptr
}

const (
	audioBufs    = 24
	audioBufSize = 9600 // 50 ms at 48 kHz stereo 16-bit
)

// player feeds PCM to the default output device through a small ring of
// buffers. If the ring is full the network is ahead of playback and the
// packet is dropped, which keeps the delay bounded.
type player struct {
	handle uintptr
	hdrs   []waveHdr
	data   []byte
	queued []bool
}

func (p *player) close() {
	if p.handle != 0 {
		pWaveOutReset.Call(p.handle)
		pWaveOutClose.Call(p.handle)
		p.handle = 0
	}
}

func (p *player) open(rate uint32) {
	p.close()
	wf := waveFormatEx{FormatTag: 1, Channels: 2, SamplesPerSec: rate, AvgBytesPerSec: rate * 4, BlockAlign: 4, BitsPerSample: 16}
	var h uintptr
	// WAVE_MAPPER = (UINT)-1
	if r, _, _ := pWaveOutOpen.Call(uintptr(unsafe.Pointer(&h)), 0xFFFFFFFF, uintptr(unsafe.Pointer(&wf)), 0, 0, 0); r != 0 {
		return
	}
	p.handle = h
	p.hdrs = make([]waveHdr, audioBufs)
	p.data = make([]byte, audioBufs*audioBufSize)
	p.queued = make([]bool, audioBufs)
	for i := range p.hdrs {
		p.hdrs[i].data = &p.data[i*audioBufSize]
		p.hdrs[i].bufferLength = audioBufSize
		pWaveOutPrepareHeader.Call(h, uintptr(unsafe.Pointer(&p.hdrs[i])), unsafe.Sizeof(p.hdrs[i]))
	}
}

// free returns an idle buffer index, or -1, and how many are still playing.
func (p *player) free() (idx, busy int) {
	idx = -1
	for i := range p.hdrs {
		if p.queued[i] && atomic.LoadUint32(&p.hdrs[i].flags)&1 != 0 { // WHDR_DONE
			p.queued[i] = false
		}
		if p.queued[i] {
			busy++
		} else if idx < 0 {
			idx = i
		}
	}
	return idx, busy
}

func (p *player) write(i int, pcm []byte) {
	copy(p.data[i*audioBufSize:(i+1)*audioBufSize], pcm)
	p.hdrs[i].bufferLength = uint32(len(pcm))
	p.queued[i] = true
	if r, _, _ := pWaveOutWrite.Call(p.handle, uintptr(unsafe.Pointer(&p.hdrs[i])), unsafe.Sizeof(p.hdrs[i])); r != 0 {
		p.queued[i] = false
	}
}

func (p *player) play(pcm []byte) {
	if p.handle == 0 {
		return
	}
	pcm = pcm[:len(pcm)&^3]
	if _, busy := p.free(); busy == 0 {
		// Starting from silence: lead with 50 ms of quiet so ordinary
		// network jitter does not cause crackles.
		if i, _ := p.free(); i >= 0 {
			p.write(i, make([]byte, audioBufSize))
		}
	} else if busy > audioBufs-8 {
		return
	}
	for len(pcm) > 0 {
		i, _ := p.free()
		if i < 0 {
			return
		}
		n := min(len(pcm), audioBufSize)
		p.write(i, pcm[:n])
		pcm = pcm[n:]
	}
}

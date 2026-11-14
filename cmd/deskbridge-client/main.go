//go:build windows

// deskbridge-client shows and controls the desktop shared by
// deskbridge-server. One window: a collapsible settings pane on the left and
// the remote screen on the right.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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

// link is one connection to a server.
type link struct {
	id   uintptr
	c    *proto.Conn
	host string
	out  chan [2][]byte // {type, payload}: input, may be dropped if stalled
	bulk chan [2][]byte // clipboard data: never dropped, yields to input
	done chan struct{}
	once sync.Once
}

func (l *link) close() { l.once.Do(func() { close(l.done); l.c.Close() }) }

type connectResult struct {
	l   *link
	err string
}

var (
	inst, hMain, hView, hFont uintptr
	dpi                       = 96

	hHost, hPort, hPass, hRemember, hConnect uintptr
	hFPS, hQuality, hAudio, hSleep, hClip    uintptr
	hVideo, hFast                            uintptr
	hVersion                                 uintptr
	statusTop                                int // where the status text starts, at 96 dpi
	hFullscreen, hStatus, hToggle            uintptr
	paneCtls                                 []uintptr

	cur        atomic.Pointer[link] // nil when not connected
	nextLinkID uintptr
	connecting bool
	connectRes atomic.Pointer[connectResult]
	audioOn    atomic.Bool
	clipOn     atomic.Bool
	statusText atomic.Pointer[string]
	videoCaps  atomic.Uint32 // codecs this PC can decode, once known
	capsKnown  atomic.Bool

	// Framebuffer. Decoders writing disjoint areas share the read lock; the
	// painter and resizes take the write lock.
	fbMu     sync.RWMutex
	fb       []byte
	fbW, fbH int

	curCursor atomic.Uintptr
	arrow     uintptr

	// Touched only on the UI thread.
	pressed      = map[uint32]kbdllHook{} // keys held on the remote PC
	localDown    = map[uint32]bool{}      // keys whose press was left to this PC
	swallowEnter bool
	swallowEnd   bool
	fullscreen   bool
	collapsed    bool
	savedStyle   uintptr
	savedRect    rect
	lastFocus    uintptr
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
	curCursor.Store(arrow)
	audioOn.Store(true)
	clipOn.Store(true)

	mainClass, viewClass := u16ptr("DeskbridgeMain"), u16ptr("DeskbridgeView")
	appIcon, _, _ := pLoadIcon.Call(inst, 1) // the icon stamped into the .exe, if any
	wc := wndClassEx{wndProc: syscall.NewCallback(mainProc), instance: inst, cursor: arrow, icon: appIcon, iconSm: appIcon,
		background: 16 /* COLOR_BTNFACE+1 */, className: mainClass}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	vc := wndClassEx{wndProc: syscall.NewCallback(viewProc), instance: inst, cursor: arrow, className: viewClass}
	vc.size = uint32(unsafe.Sizeof(vc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&vc)))

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
	hView, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(viewClass)), 0, wsChild|wsVisible, 0, 0, 10, 10, hMain, 0, inst, 0)
	loadSettings()
	startClipboard()
	user32.NewProc("SetTimer").Call(hMain, 1, 1000, 0) // drives the frame-rate readout
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
		setText(hHost, host)
		if len(args) >= 2 {
			setText(hPass, args[1])
		}
	}
	// The pane starts collapsed. With a computer and password already known
	// we connect straight away; otherwise the pane opens so they can be
	// entered.
	ready := getText(hHost) != "" && getText(hPass) != ""
	collapsed = ready
	layout()
	pShowWindow.Call(hMain, 1)
	if ready {
		pSetFocus.Call(hView)
		startConnect()
	} else {
		pSetFocus.Call(hHost)
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
	if l := cur.Load(); l != nil {
		l.close()
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
	combo := func(id int, choices []int, suffix string) uintptr {
		h := control("COMBOBOX", "", cbsDropDownList|wsVScroll|wsTabStop, 0, m, y, w, 240, id)
		for _, c := range choices {
			pSendMessage.Call(h, 0x0143, 0, uintptr(unsafe.Pointer(u16ptr(strconv.Itoa(c)+suffix)))) // CB_ADDSTRING
		}
		y += 36
		return h
	}
	check := func(id int, text string) uintptr {
		h := control("BUTTON", text, bsAutoCheck|wsTabStop, 0, m, y, w, 22, id)
		y += 28
		return h
	}

	label("Computer name or IP address")
	hHost = edit(0)
	label("Port")
	hPort = edit(esNumber)
	label("Password")
	hPass = edit(esPassword)
	hRemember = check(idRemember, "Remember password")
	y += 4
	hConnect = control("BUTTON", "Connect", bsDefPush|wsTabStop, 0, m, y, w, 32, idConnect)
	y += 50

	label("Remote display")
	hMonitor = control("COMBOBOX", "", cbsDropDownList|wsVScroll|wsTabStop, 0, m, y, w, 240, idMonitor)
	pEnableWindow.Call(hMonitor, 0)
	y += 36
	label("Frames per second")
	hFPS = combo(idFPS, fpsChoices, "")
	label("Picture quality (photos and video)")
	hQuality = combo(idQuality, qualityChoices, " %")
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
	pMoveWindow.Call(hView, uintptr(x), 0, uintptr(max(cw-x, 0)), uintptr(ch), 1)

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
	if cur.Load() != nil {
		server = "version not reported" // servers older than 1.1.0 do not say
		if v := serverVersion.Load(); v != nil {
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

func settingsPayload() []byte {
	b := []byte{byte(comboValue(hFPS, fpsChoices)), byte(comboValue(hQuality, qualityChoices)), 0, 0, 0, 0, 0, 0}
	if checked(hAudio) {
		b[2] = 1
	}
	if checked(hSleep) {
		b[3] = 1
	}
	if checked(hClip) {
		b[4] = 1
	}
	if checked(hVideo) {
		b[5] = 1
	}
	b[6] = byte(videoCaps.Load())
	if checked(hFast) {
		b[7] = 1
	}
	return b
}

// applySettings pushes the pane's choices to the server and remembers them.
func applySettings() {
	audioOn.Store(checked(hAudio))
	clipOn.Store(checked(hClip))
	queue(proto.MsgSettings, settingsPayload())
	saveSettings()
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
		"host=" + getText(hHost),
		"port=" + getText(hPort),
		"fps=" + strconv.Itoa(comboValue(hFPS, fpsChoices)),
		"quality=" + strconv.Itoa(comboValue(hQuality, qualityChoices)),
		"audio=" + b(checked(hAudio)),
		"sleep=" + b(checked(hSleep)),
		"clipboard=" + b(checked(hClip)),
		"video=" + b(checked(hVideo)),
		"fastcapture=" + b(checked(hFast)),
		"display=" + strconv.Itoa(wantedDisplay),
		"remember=" + b(checked(hRemember)),
	}
	if checked(hRemember) {
		if enc := dpapi(pCryptProtectData, []byte(getText(hPass))); enc != nil {
			lines = append(lines, "password="+hex.EncodeToString(enc))
		}
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0600)
}

func loadSettings() {
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
				wantedDisplay = n
			}
		case "remember":
			setChecked(hRemember, v == "1")
		case "password":
			if enc, err := hex.DecodeString(v); err == nil {
				if pw := dpapi(pCryptUnprotectData, enc); pw != nil {
					setText(hPass, string(pw))
				}
			}
		}
	}
	audioOn.Store(checked(hAudio))
	clipOn.Store(checked(hClip))
}

// ---------- connecting ----------

func startConnect() {
	if connecting || cur.Load() != nil {
		return
	}
	host, port, password := getText(hHost), getText(hPort), getText(hPass)
	if host == "" || password == "" {
		setStatus("Enter the computer name and the password first.")
		return
	}
	if port == "" {
		port = proto.DefaultPort
	}
	connecting = true
	pEnableWindow.Call(hConnect, 0)
	setStatus("Connecting to " + host + "...")
	nextLinkID++
	id := nextLinkID
	go func() {
		res := &connectResult{}
		addr := net.JoinHostPort(host, port)
		nc, err := net.DialTimeout("tcp", addr, 8*time.Second)
		if err != nil {
			res.err = "Could not reach " + addr + ".\r\n\r\nCheck that Deskbridge is running on that PC and that both PCs are on the same network."
		} else {
			if tc, ok := nc.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			tc, err := proto.ClientHandshake(nc, proto.DeriveKey(password))
			if errors.Is(err, proto.ErrAuth) {
				nc.Close()
				res.err = "Wrong password."
			} else if err != nil {
				nc.Close()
				res.err = "Connection failed: " + err.Error()
			} else {
				res.l = &link{id: id, c: proto.NewConn(tc), host: host, out: make(chan [2][]byte, 2048), bulk: make(chan [2][]byte, 2), done: make(chan struct{})}
			}
		}
		connectRes.Store(res)
		pPostMessage.Call(hMain, wmConnectDone, 0, 0)
	}()
}

func onConnectDone() {
	res := connectRes.Swap(nil)
	if res == nil {
		return
	}
	connecting = false
	pEnableWindow.Call(hConnect, 1)
	if res.l == nil {
		setStatus(res.err)
		openPane() // so the reason is visible
		return
	}
	l := res.l
	cur.Store(l)
	go sender(l)
	go receiver(l)
	applySettings()
	setText(hConnect, "Disconnect")
	refreshVersionLine()
	displayRequested = false
	pEnableWindow.Call(hSend, 1)
	pEnableWindow.Call(hFullscreen, 1)
	setStatus("Connected to " + l.host + ".\r\n\r\nClick the remote screen to type on the remote PC. Click in this pane to get your own keyboard back." + videoNote())
	pSetFocus.Call(hView)
}

func onDisconnected(id uintptr) {
	l := cur.Load()
	if l == nil || l.id != id {
		return
	}
	cur.Store(nil)
	l.close()
	clear(pressed)
	if fullscreen {
		toggleFullscreen()
	}
	fbMu.Lock()
	fb, fbW, fbH = nil, 0, 0
	fbMu.Unlock()
	curCursor.Store(arrow)
	setText(hConnect, "Connect")
	pEnableWindow.Call(hSend, 0)
	remoteDisplays.Store(nil)
	showDisplays()
	pEnableWindow.Call(hFullscreen, 0)
	serverVersion.Store(nil)
	setTitle("Deskbridge")
	if updateNote != "" && time.Since(updateNoteAt) < 3*time.Minute {
		// Expected: the remote PC is restarting for its update.
		setStatus(updateNote + "\r\n\r\nDisconnected. Click Connect in a few seconds.")
	} else {
		setStatus("Disconnected.")
	}
	refreshUpdate()
	pInvalidateRect.Call(hView, 0, 0)
	openPane() // so Connect is within reach
}

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
	statusText.Store(&s)
	pPostMessage.Call(hMain, wmVideoFailed, 0, 0)
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
		fbMu.RLock()
		if fb != nil && fbW >= w && fbH >= h {
			proto.NV12RectsToBGRA(fb, fbW, y, uv, stride, w, h, areas)
		}
		fbMu.RUnlock()
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
		postStatus(v.report())
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
	c := &winclip.Sync{Enabled: clipOn.Load, Status: postStatus}
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
				fbMu.RLock()
				if fb != nil {
					proto.DecodeRect(j.enc, j.data, j.r, fb, fbW, fbH)
				}
				fbMu.RUnlock()
				wg.Done()
			}
		}()
	}
	defer close(jobs)
	var audio player
	defer audio.close()
	video := videoSink{host: l.host, restart: func() {
		select {
		case l.bulk <- [2][]byte{{proto.MsgVideoRestart}, nil}:
		case <-l.done:
		}
	}}
	defer video.close()
	// Clipboard data is never dropped, but waits its turn behind input.
	var peer *winclip.Peer
	if clip != nil {
		peer = clip.Connect(func(t byte, p []byte) error {
			select {
			case l.bulk <- [2][]byte{{t}, p}:
				return nil
			case <-l.done:
				return errors.New("disconnected")
			}
		})
		defer peer.Close()
	}

	for {
		t, p, err := l.c.Read()
		if err != nil {
			return
		}
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
			fbMu.Lock()
			fb, fbW, fbH = make([]byte, w*h*4), w, h
			fbMu.Unlock()
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
		case proto.MsgMonitors:
			if d := parseDisplays(p); d != nil {
				remoteDisplays.Store(d)
				pPostMessage.Call(hMain, wmMonitors, 0, 0)
			}
		case proto.MsgUpdateResult:
			select {
			case updateResult <- p:
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
				serverVersion.Store(&v)
				pPostMessage.Call(hMain, wmUpdateInfo, 0, 0)
			case proto.NoticeGeneral:
				postStatus(string(p[1:]))
			case proto.NoticeCapture:
				video.capture = string(p[1:])
				postStatus(video.report())
			}
		case proto.MsgVideo:
			wg.Wait() // anything drawn before this frame must land first
			video.handle(p)
		case proto.MsgFrameEnd:
			framesShown.Add(1)
			wg.Wait()
			pPostMessage.Call(hView, wmRepaint, 0, 0)
			select { // must not be dropped: the server waits for it
			case l.out <- [2][]byte{{proto.MsgAck}, nil}:
			case <-l.done:
				return
			}
		case proto.MsgCursor:
			if len(p) >= 2 {
				if h, _, _ := pLoadCursor.Call(0, uintptr(binary.LittleEndian.Uint16(p))); h != 0 {
					curCursor.Store(h)
					pPostMessage.Call(hView, wmCursor, 0, 0)
				}
			}
		case proto.MsgAudioFmt:
			if len(p) >= 4 {
				audio.open(binary.LittleEndian.Uint32(p))
			}
		case proto.MsgAudio:
			if audioOn.Load() {
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
	if message == wmMonitors {
		showDisplays()
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
			if l := cur.Load(); l != nil {
				// Enter anywhere in the pane also arrives here; only a
				// deliberate press of the button itself disconnects.
				if f, _, _ := pGetFocus.Call(); f == hConnect {
					l.close() // the receiver notices and reports back
				}
			} else {
				startConnect()
			}
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
		}
	case wmSetFocus:
		// A plain window does not hand focus back to a child by itself.
		target := lastFocus
		if v, _, _ := pIsWindowVisible.Call(target); target == 0 || target == h || v == 0 {
			target = hHost
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
			fbMu.RLock()
			title := fmt.Sprintf("Deskbridge - %s (%d x %d)", l.host, fbW, fbH)
			fbMu.RUnlock()
			setTitle(title)
			pInvalidateRect.Call(hView, 0, 0)
		}
		return 0
	case wmConnectDone:
		onConnectDone()
		return 0
	case wmVideoCaps:
		if cur.Load() != nil {
			applySettings() // tell the server what we can decode
			if note := videoNote(); note != "" {
				setStatus(strings.TrimSpace(note))
			}
		}
		return 0
	case wmVideoFailed:
		if checked(hVideo) {
			setChecked(hVideo, false)
			applySettings()
		}
		if s := statusText.Swap(nil); s != nil {
			setStatus(*s)
		}
		return 0
	case wmStatus:
		if s := statusText.Swap(nil); s != nil && cur.Load() != nil {
			text := *s
			if strings.HasPrefix(text, "Connected to ") {
				text += videoNote() // e.g. that this PC lacks an H.265 decoder
			}
			setStatus(text)
		}
		return 0
	case wmDisconnected:
		onDisconnected(wParam)
		return 0
	case 0x0113: // WM_TIMER, once a second
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
	fbMu.RLock()
	rw, rh = fbW, fbH
	fbMu.RUnlock()
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
	cx, cy := int(int16(lParam)), int(int16(lParam>>16))
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
		if lParam&0xFFFF == 1 && cur.Load() != nil { // HTCLIENT
			pSetCursor.Call(curCursor.Load())
			return 1
		}
	case wmCursor:
		var pt [2]int32
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		pScreenToClient.Call(h, uintptr(unsafe.Pointer(&pt)))
		var rc rect
		pGetClientRect.Call(h, uintptr(unsafe.Pointer(&rc)))
		if fg, _, _ := pGetForegroundWindow.Call(); fg == hMain && pt[0] >= 0 && pt[1] >= 0 && pt[0] < rc.right && pt[1] < rc.bottom {
			pSetCursor.Call(curCursor.Load())
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
	fbMu.Lock()
	defer fbMu.Unlock()
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
	if _, remote := pressed[id]; up && !remote && !(k.vk == 0x0D && swallowEnter) && !(k.vk == 0x23 && swallowEnd) {
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
	held := func(vks ...uint32) bool {
		for _, v := range vks {
			for _, p := range pressed {
				if p.vk == v {
					return true
				}
			}
		}
		return false
	}
	if k.vk == vkEnd {
		// Ctrl+Alt+End stands in for Ctrl+Alt+Del, which Windows always
		// keeps for this PC.
		if !up && held(vkLCtrl, vkRCtrl) && held(vkLAlt, vkRAlt) {
			if !swallowEnd {
				swallowEnd = true
				queue(proto.MsgAction, []byte{proto.ActionCtrlAltDel})
			}
			return 1
		}
		if up && swallowEnd {
			swallowEnd = false
			return 1
		}
	}
	if k.vk == vkReturn {
		if !up && held(vkLCtrl, vkRCtrl) && held(vkLAlt, vkRAlt) {
			if !swallowEnter {
				swallowEnter = true
				pPostMessage.Call(hMain, wmFullscreen, 0, 0)
			}
			return 1
		}
		if up && swallowEnter {
			swallowEnter = false
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

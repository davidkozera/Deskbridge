//go:build windows

// deskbridge-setup installs, updates and removes Deskbridge.
//
// One setup program serves both PCs. It carries the server and the client
// for both processor types, installs whichever parts are ticked using the
// right one for the PC it is run on, and sets everything up: the server's
// startup service and firewall rule, the client's Start menu shortcut, and
// an entry under "Installed apps" for removing it again.
package main

import (
	"compress/gzip"
	"embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

//go:embed payload/*
var payload embed.FS

const (
	serviceName  = "Deskbridge"
	uninstallKey = `HKLM\Software\Microsoft\Windows\CurrentVersion\Uninstall\Deskbridge`
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")

	pSetDpiCtx        = user32.NewProc("SetProcessDpiAwarenessContext")
	pGetDpiForWindow  = user32.NewProc("GetDpiForWindow")
	pRegisterClassEx  = user32.NewProc("RegisterClassExW")
	pCreateWindowEx   = user32.NewProc("CreateWindowExW")
	pDefWindowProc    = user32.NewProc("DefWindowProcW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pEnableWindow     = user32.NewProc("EnableWindow")
	pGetMessage       = user32.NewProc("GetMessageW")
	pIsDialogMessage  = user32.NewProc("IsDialogMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessage  = user32.NewProc("DispatchMessageW")
	pPostMessage      = user32.NewProc("PostMessageW")
	pSendMessage      = user32.NewProc("SendMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pLoadCursor       = user32.NewProc("LoadCursorW")
	pLoadIcon         = user32.NewProc("LoadIconW")
	pSetWindowText    = user32.NewProc("SetWindowTextW")
	pSetFocus         = user32.NewProc("SetFocus")
	pMessageBox       = user32.NewProc("MessageBoxW")
	pAdjustWindowRect = user32.NewProc("AdjustWindowRectExForDpi")

	pCreateFont = gdi32.NewProc("CreateFontW")

	pGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	pCreateActCtx     = kernel32.NewProc("CreateActCtxW")
	pActivateActCtx   = kernel32.NewProc("ActivateActCtx")
	pIsWow64Process2  = kernel32.NewProc("IsWow64Process2")
	pGetCurrentProces = kernel32.NewProc("GetCurrentProcess")

	pOpenSCManager      = advapi32.NewProc("OpenSCManagerW")
	pOpenService        = advapi32.NewProc("OpenServiceW")
	pControlService     = advapi32.NewProc("ControlService")
	pQueryServiceStatus = advapi32.NewProc("QueryServiceStatus")
	pCloseServiceHandle = advapi32.NewProc("CloseServiceHandle")

	pShellExecute       = shell32.NewProc("ShellExecuteW")
	pInitCommonControls = comctl32.NewProc("InitCommonControls")
)

const (
	wmDestroy = 0x0002
	wmSetFont = 0x0030
	wmCommand = 0x0111
	wmApp     = 0x8000

	wmProgress      = wmApp + 1 // show the text left in statusText
	wmDone          = wmApp + 2 // the work finished; wParam 1 = uninstalled
	wmPublicNetwork = wmApp + 3 // this PC's network turned out to be a Public one

	wsCaption    = 0x00C00000
	wsSysMenu    = 0x00080000
	wsMinimize   = 0x00020000
	wsChild      = 0x40000000
	wsVisible    = 0x10000000
	wsTabStop    = 0x00010000
	wsVScroll    = 0x00200000
	wsClipChild  = 0x02000000
	esMultiline  = 0x0004
	esReadOnly   = 0x0800
	esAutoVScrol = 0x0040

	idInstall   = 1
	idClose     = 2
	idServer    = 101
	idClient    = 102
	idUninstall = 103
	idOpen      = 104
	idPublic    = 105

	clientW, clientH = 520, 500 // at 96 dpi
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

var (
	inst, hMain, hFont, hBold                    uintptr
	hServer, hClient, hInstall, hUninstall, hOut uintptr
	hOpen, hClose, hPublic                       uintptr
	dpi                                          = 96
	busy                                         bool
	statusText                                   atomic.Pointer[string]
)

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
func sc(v int) int         { return v * dpi / 96 }

func setText(h uintptr, s string) { pSetWindowText.Call(h, uintptr(unsafe.Pointer(u16(s)))) }
func checked(h uintptr) bool      { r, _, _ := pSendMessage.Call(h, 0x00F0, 0, 0); return r == 1 }
func setChecked(h uintptr, on bool) {
	v := uintptr(0)
	if on {
		v = 1
	}
	pSendMessage.Call(h, 0x00F1, v, 0)
}

func installDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "Deskbridge")
}

func serverPath() string { return filepath.Join(installDir(), "deskbridge-server.exe") }
func clientPath() string { return filepath.Join(installDir(), "deskbridge-client.exe") }
func setupPath() string  { return filepath.Join(installDir(), "deskbridge-setup.exe") }

func shortcutPath() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, `Microsoft\Windows\Start Menu\Programs\Deskbridge.lnk`)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// nativeArch is the PC's real processor type. This program is built for x64
// and runs under emulation on Arm PCs, so its own type says nothing.
func nativeArch() string {
	if pIsWow64Process2.Find() == nil {
		var process, native uint16
		self, _, _ := pGetCurrentProces.Call()
		if r, _, _ := pIsWow64Process2.Call(self, uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native))); r != 0 && native == 0xAA64 {
			return "arm64"
		}
	}
	return "amd64"
}

// isAdmin reports whether we may manage services, by trying to.
func isAdmin() bool {
	scm, _, _ := pOpenSCManager.Call(0, 0, 0xF003F)
	if scm == 0 {
		return false
	}
	pCloseServiceHandle.Call(scm)
	return true
}

func main() {
	// "/silent" updates whatever is installed without showing a window; it
	// is how the client's Update button and the server's self-update run
	// setup. "/relaunch" reopens the client afterwards.
	silent, relaunch := false, false
	for _, a := range os.Args[1:] {
		switch strings.ToLower(strings.TrimLeft(a, "/-")) {
		case "silent":
			silent = true
		case "relaunch":
			relaunch = true
		}
	}
	if !isAdmin() {
		// Start again with administrator rights; Windows asks for approval.
		exe, _ := os.Executable()
		args := strings.Join(os.Args[1:], " ")
		pShellExecute.Call(0, uintptr(unsafe.Pointer(u16("runas"))), uintptr(unsafe.Pointer(u16(exe))),
			uintptr(unsafe.Pointer(u16(args))), 0, 1)
		return
	}
	if silent {
		os.Exit(runSilent(relaunch))
	}
	runtime.LockOSThread()
	pSetDpiCtx.Call(^uintptr(3))
	enableVisualStyles()
	inst, _, _ = pGetModuleHandle.Call(0)
	arrow, _, _ := pLoadCursor.Call(0, 32512)
	icon, _, _ := pLoadIcon.Call(inst, 1)

	class := u16("DeskbridgeSetup")
	wc := wndClassEx{wndProc: syscall.NewCallback(wndProc), instance: inst, cursor: arrow, icon: icon, iconSm: icon,
		background: 16 /* COLOR_BTNFACE+1 */, className: class}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	const style = wsCaption | wsSysMenu | wsMinimize | wsClipChild
	const cwUseDefault = 0x80000000
	hMain, _, _ = pCreateWindowEx.Call(0x00010000, uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(u16("Deskbridge "+proto.Version+" Setup"))), style, cwUseDefault, cwUseDefault, 560, 520, 0, 0, inst, 0)
	if hMain == 0 {
		return
	}
	if d, _, _ := pGetDpiForWindow.Call(hMain); d != 0 {
		dpi = int(d)
	}
	// Size the window so its inside is exactly the layout below.
	rc := [4]int32{0, 0, int32(sc(clientW)), int32(sc(clientH))}
	if pAdjustWindowRect.Find() == nil {
		pAdjustWindowRect.Call(uintptr(unsafe.Pointer(&rc)), style, 0, 0x00010000, uintptr(dpi))
	}
	user32.NewProc("SetWindowPos").Call(hMain, 0, 0, 0, uintptr(rc[2]-rc[0]), uintptr(rc[3]-rc[1]), 0x0002|0x0004) // no move, no z-order

	font := func(points, weight int) uintptr {
		h := -int32(points * dpi / 72)
		f, _, _ := pCreateFont.Call(uintptr(h), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(u16("Segoe UI"))))
		return f
	}
	hFont, hBold = font(9, 400), font(13, 600)
	build()

	if len(os.Args) > 1 && strings.EqualFold(strings.TrimLeft(os.Args[1], "/-"), "uninstall") {
		setChecked(hServer, false)
		setChecked(hClient, false)
		startWork(true)
	}
	pShowWindow.Call(hMain, 1)

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		if r, _, _ := pIsDialogMessage.Call(hMain, uintptr(unsafe.Pointer(&m))); r != 0 {
			continue
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// enableVisualStyles gives the buttons the modern Windows look by borrowing
// the common-controls manifest that ships inside shell32.dll.
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
	}{flags: 0x8, source: u16(root + `\System32\shell32.dll`), resName: 124}
	ctx.size = uint32(unsafe.Sizeof(ctx))
	h, _, _ := pCreateActCtx.Call(uintptr(unsafe.Pointer(&ctx)))
	if h != 0 && h != ^uintptr(0) {
		var cookie uintptr
		pActivateActCtx.Call(h, uintptr(unsafe.Pointer(&cookie)))
	}
	pInitCommonControls.Call()
}

func control(class, text string, style, exStyle uintptr, x, y, w, h, id int, font uintptr) uintptr {
	hw, _, _ := pCreateWindowEx.Call(exStyle, uintptr(unsafe.Pointer(u16(class))), uintptr(unsafe.Pointer(u16(text))),
		style|wsChild|wsVisible, uintptr(sc(x)), uintptr(sc(y)), uintptr(sc(w)), uintptr(sc(h)), hMain, uintptr(id), inst, 0)
	pSendMessage.Call(hw, wmSetFont, font, 1)
	return hw
}

func build() {
	const m, w = 20, clientW - 40
	control("STATIC", "Deskbridge "+proto.Version, 0, 0, m, 16, w, 28, 0, hBold)
	control("STATIC", "Choose what to install on this PC. Installing over an older version updates it and keeps its password and settings.", 0, 0, m, 50, w, 36, 0, hFont)
	hServer = control("BUTTON", "Server: let this PC be controlled from another PC", 0x3|wsTabStop, 0, m, 96, w, 22, idServer, hFont)
	control("STATIC", "Starts with Windows and works at the lock screen.", 0, 0, m+18, 118, w-18, 18, 0, hFont)
	hPublic = control("BUTTON", "Also allow connections while on a public network", 0x3|wsTabStop, 0, m+18, 140, w-18, 22, idPublic, hFont)
	hClient = control("BUTTON", "Client: control another PC from this one", 0x3|wsTabStop, 0, m, 176, w, 22, idClient, hFont)
	control("STATIC", "Adds Deskbridge to the Start menu.", 0, 0, m+18, 198, w-18, 18, 0, hFont)
	hOut = control("EDIT", "", esMultiline|esReadOnly|esAutoVScrol|wsVScroll, 0x200, m, 230, w, 196, 0, hFont)
	hInstall = control("BUTTON", "Install", 0x1|wsTabStop, 0, m, 444, 110, 32, idInstall, hFont)
	hUninstall = control("BUTTON", "Uninstall", wsTabStop, 0, m+122, 444, 110, 32, idUninstall, hFont)
	hOpen = control("BUTTON", "Open Deskbridge", wsTabStop, 0, m+244, 444, 124, 32, idOpen, hFont)
	hClose = control("BUTTON", "Close", wsTabStop, 0, m+w-100, 444, 100, 32, idClose, hFont)
	setChecked(hPublic, exists(publicMarker()))
	// Windows Firewall treats a network as Public or Private. If this PC's
	// network is Public, a rule for private networks alone would leave the
	// server unreachable, so offer the wider rule up front.
	go func() {
		if strings.Contains(networkCategories(), "Public") {
			pPostMessage.Call(hMain, wmPublicNetwork, 0, 0)
		}
	}()

	// Start from what is already installed; on a fresh PC, guess from its
	// processor: Arm PCs are usually the one being controlled.
	hasServer, hasClient := exists(serverPath()), exists(clientPath())
	if !hasServer && !hasClient {
		hasServer = nativeArch() == "arm64"
		hasClient = !hasServer
	}
	setChecked(hServer, hasServer)
	setChecked(hClient, hasClient)
	refresh()
	arch := "an x64"
	if nativeArch() == "arm64" {
		arch = "an Arm"
	}
	setText(hOut, "This PC has "+arch+" processor; the matching programs will be installed to:\r\n"+installDir())
	pSetFocus.Call(hInstall)
}

// refresh sets which buttons can be pressed.
func refresh() {
	on := func(h uintptr, yes bool) {
		v := uintptr(0)
		if yes {
			v = 1
		}
		pEnableWindow.Call(h, v)
	}
	installed := exists(serverPath()) || exists(clientPath())
	on(hInstall, !busy && (checked(hServer) || checked(hClient)))
	on(hUninstall, !busy && installed)
	on(hOpen, !busy && exists(clientPath()))
	on(hServer, !busy)
	on(hPublic, !busy && checked(hServer))
	on(hClient, !busy)
	on(hClose, !busy)
	if installed {
		setText(hInstall, "Install / update")
	} else {
		setText(hInstall, "Install")
	}
}

func wndProc(h, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmCommand:
		id, code := int(wParam&0xFFFF), int(wParam>>16&0xFFFF)
		if code != 0 {
			break
		}
		switch id {
		case idInstall:
			if !busy && (checked(hServer) || checked(hClient)) {
				startWork(false)
			}
		case idUninstall:
			const yes = 6
			r, _, _ := pMessageBox.Call(h, uintptr(unsafe.Pointer(u16("Remove Deskbridge from this PC, including the server's password?"))),
				uintptr(unsafe.Pointer(u16("Deskbridge Setup"))), 0x04|0x30) // Yes/No, warning
			if r == yes && !busy {
				startWork(true)
			}
		case idOpen:
			// Through Explorer, so that it does not inherit our
			// administrator rights.
			exec.Command("explorer.exe", clientPath()).Start()
		case idServer, idClient:
			refresh()
		case idClose:
			if !busy {
				pDestroyWindow.Call(h)
			}
		}
		return 0
	case wmPublicNetwork:
		// Say so, but leave the choice to the person at the PC.
		setText(hPublic, "Also allow connections while on a public network (this one is public)")
		return 0
	case wmProgress:
		if s := statusText.Swap(nil); s != nil {
			setText(hOut, *s)
		}
		return 0
	case wmDone:
		busy = false
		if s := statusText.Swap(nil); s != nil {
			setText(hOut, *s)
		}
		refresh()
		uninstalled = wParam == 1
		return 0
	case wmDestroy:
		if uninstalled {
			// Our own copy in the install folder cannot delete itself while
			// running, so a helper removes the folder a moment after we exit.
			removeFolderLater()
		}
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(h, message, wParam, lParam)
	return r
}

// ---------- doing the work ----------

var log strings.Builder

func say(format string, a ...any) {
	fmt.Fprintf(&log, format+"\r\n", a...)
	if hMain == 0 {
		return // silent: the log is written to a file at the end
	}
	s := log.String()
	statusText.Store(&s)
	pPostMessage.Call(hMain, wmProgress, 0, 0)
}

// runSilent updates the parts of Deskbridge that are installed on this PC,
// with no window. What happened is appended to update.log in the install
// folder.
func runSilent(relaunch bool) int {
	server, client := exists(serverPath()), exists(clientPath())
	allowPublic = exists(publicMarker()) // an update keeps the choice made at install
	ok := false
	if !server && !client {
		say("Deskbridge is not installed on this PC, so there is nothing to update.")
	} else {
		ok = install(server, client)
	}
	if f, err := os.OpenFile(filepath.Join(installDir(), "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		fmt.Fprintf(f, "---- %s: update to %s ----\r\n%s\r\n", time.Now().Format("2006-01-02 15:04:05"), proto.Version, log.String())
		f.Close()
	}
	if relaunch && client {
		// Through Explorer, so that it does not inherit administrator rights.
		exec.Command("explorer.exe", clientPath()).Start()
	}
	if ok {
		return 0
	}
	return 1
}

// startService starts the Deskbridge service again. Used when replacing its
// program failed, so that a failed update does not leave the PC unreachable.
func startService() {
	scm, _, _ := pOpenSCManager.Call(0, 0, 0xF003F)
	if scm == 0 {
		return
	}
	defer pCloseServiceHandle.Call(scm)
	svc, _, _ := pOpenService.Call(scm, uintptr(unsafe.Pointer(u16(serviceName))), 0x0010) // start
	if svc == 0 {
		return
	}
	defer pCloseServiceHandle.Call(svc)
	advapi32.NewProc("StartServiceW").Call(svc, 0, 0)
}

func startWork(uninstalling bool) {
	busy = true
	refresh()
	log.Reset()
	server, client := checked(hServer), checked(hClient)
	allowPublic = checked(hPublic)
	go func() {
		done := uintptr(0)
		if uninstalling {
			uninstall()
			done = 1
		} else {
			install(server, client)
		}
		s := log.String()
		statusText.Store(&s)
		pPostMessage.Call(hMain, wmDone, done, 0)
	}()
}

// hidden runs a command without flashing a console window.
func hidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// extract writes one of the built-in programs to dst, replacing what is
// there. A program that is still running cannot be replaced, so it retries
// for a while to give a stopping one time to exit.
func extract(name, dst string) error {
	f, err := payload.Open("payload/" + name + ".gz")
	if err != nil {
		return errors.New("this setup program was built without " + name)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, gz); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	for try := 0; ; try++ {
		os.Remove(dst)
		if err = os.Rename(tmp, dst); err == nil {
			return nil
		}
		if try >= 40 {
			os.Remove(tmp)
			return fmt.Errorf("%s is in use and could not be replaced. Close it and try again", filepath.Base(dst))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ---------- Windows Firewall ----------

const (
	firewallRule       = "Deskbridge server"
	firewallRulePublic = "Deskbridge server (public networks)"
)

// allowPublic is whether the rule should cover public networks as well.
var allowPublic bool

// publicMarker is a file whose presence records that choice, so updates
// (which run without a window) keep it.
func publicMarker() string { return filepath.Join(installDir(), "allow-public-networks") }

// raw runs a command line exactly as written. netsh reads its command line
// its own way, so quoting is spelled out instead of left to the usual rules.
func raw(program, commandLine string) (string, error) {
	cmd := exec.Command(program)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000, CmdLine: program + " " + commandLine}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func powershell(script string) (string, error) {
	return hidden("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
}

// networkCategories is how Windows classes the networks this PC is on right
// now, such as "Private" or "Public", comma-separated; empty if unknown.
func networkCategories() string {
	out, err := powershell("(Get-NetConnectionProfile | ForEach-Object { $_.NetworkCategory }) -join ','")
	if err != nil {
		return ""
	}
	return out
}

// firewall replaces the server's Windows Firewall rules, checks that they
// are really there, and says what it found. It reports whether the server
// can be reached through the firewall.
//
// There are up to two rules: one for private and work networks, and, if the
// person asked for it, one for public networks that only lets in PCs on the
// same local network.
func firewall() bool {
	if hMain == 0 {
		// An update with no window: rules that are there already are right
		// (the program has not moved), so leave a working setup alone.
		if _, missing := raw("netsh.exe", `advfirewall firewall show rule name="`+firewallRule+`"`); missing == nil {
			say("  Windows Firewall: rule \"%s\" is in place.", firewallRule)
			return true
		}
	}
	if allowPublic {
		os.WriteFile(publicMarker(), []byte("yes\r\n"), 0644)
	} else {
		os.Remove(publicMarker())
	}
	removeFirewallRules()
	okPrivate, why := addFirewallRule(firewallRule, "Private,Domain", "")
	if !okPrivate {
		say("  Windows Firewall: the rule could NOT be added, so other PCs will be blocked.")
		if why != "" {
			say("  Windows said: %s", why)
		}
		say("  To fix it by hand: Windows Security > Firewall & network protection > Allow an app")
		say("  through firewall > add %s", serverPath())
		return false
	}
	say("  Windows Firewall: rule \"%s\" added and checked (private and work networks).", firewallRule)
	// A rule that blocks the program wins over one that allows it. Windows
	// makes such rules when its "allow access?" question is cancelled.
	if blocking, err := powershell("(Get-NetFirewallApplicationFilter -Program '" + serverPath() + "' -ErrorAction SilentlyContinue | " +
		"Get-NetFirewallRule | Where-Object { $_.Action -eq 'Block' -and $_.Enabled -eq 'True' -and $_.Direction -eq 'Inbound' } | " +
		"ForEach-Object { $_.DisplayName } | Select-Object -Unique) -join '; '"); err == nil && blocking != "" {
		say("  BUT another firewall rule blocks the server and takes priority: %s", blocking)
		say("  Remove it in Windows Defender Firewall with Advanced Security > Inbound Rules.")
	}
	public := strings.Contains(networkCategories(), "Public")
	if allowPublic {
		if ok, why := addFirewallRule(firewallRulePublic, "Public", "LocalSubnet"); ok {
			say("  Windows Firewall: rule \"%s\" added and checked", firewallRulePublic)
			say("  (PCs on the same network only).")
		} else {
			say("  Windows Firewall: the rule for public networks could NOT be added. %s", why)
			return !public
		}
	} else if public {
		say("  BUT this PC's network is set to Public, so Windows will still block the server here.")
		say("  Either tick \"Also allow connections while on a public network\" and click Install again,")
		say("  or set the network to Private (Settings > Network & internet > your connection).")
	}
	return true
}

func removeFirewallRules() {
	for _, name := range []string{firewallRule, firewallRulePublic} {
		raw("netsh.exe", `advfirewall firewall delete rule name="`+name+`"`)
	}
}

// addFirewallRule lets the server program accept connections on its port on
// the given kinds of network, from the given addresses ("" for any). It
// tries two ways of making the rule, so that one being unavailable or
// refused does not leave the PC without one, then looks for the rule rather
// than trusting either. On failure it returns what Windows said.
func addFirewallRule(name, profiles, from string) (bool, string) {
	ps := "$ErrorActionPreference='Stop'; New-NetFirewallRule -DisplayName '" + name + "' -Direction Inbound -Action Allow -Program '" +
		serverPath() + "' -Protocol TCP -LocalPort " + proto.DefaultPort + " -Profile " + profiles
	ns := `advfirewall firewall add rule name="` + name + `" dir=in action=allow program="` + serverPath() +
		`" protocol=TCP localport=` + proto.DefaultPort + ` profile=` + strings.ToLower(profiles) + ` enable=yes`
	if from != "" {
		ps += " -RemoteAddress " + from
		ns += " remoteip=" + strings.ToLower(from)
	}
	why, err := powershell(ps + " | Out-Null")
	if err != nil {
		var out string
		if out, err = raw("netsh.exe", ns); err != nil {
			why = strings.Join(strings.Fields(why+" "+out), " ")
		}
	}
	if _, missing := raw("netsh.exe", `advfirewall firewall show rule name="`+name+`"`); missing != nil {
		if len(why) > 300 {
			why = why[:300] + "..."
		}
		return false, why
	}
	return true, ""
}

// stopService stops the Deskbridge service, if there is one, and waits for
// it, so that its program can be replaced.
func stopService() {
	scm, _, _ := pOpenSCManager.Call(0, 0, 0xF003F)
	if scm == 0 {
		return
	}
	defer pCloseServiceHandle.Call(scm)
	svc, _, _ := pOpenService.Call(scm, uintptr(unsafe.Pointer(u16(serviceName))), 0x0020|0x0004) // stop, query
	if svc == 0 {
		return
	}
	defer pCloseServiceHandle.Call(svc)
	var st [7]uint32
	pControlService.Call(svc, 1, uintptr(unsafe.Pointer(&st)))
	for i := 0; i < 60; i++ {
		if r, _, _ := pQueryServiceStatus.Call(svc, uintptr(unsafe.Pointer(&st))); r == 0 || st[1] == 1 { // stopped
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
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

// install puts the chosen parts in place and reports whether all went well.
func install(server, client bool) bool {
	arch, dir := nativeArch(), installDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		say("Could not create %s: %v", dir, err)
		return false
	}
	failed := false

	if server {
		say("Installing the server...")
		stopService()
		if err := extract("server-"+arch+".exe", serverPath()); err != nil {
			say("  Failed: %v", err)
			failed = true
			startService() // the previous version is still in place: bring it back
		} else {
			// The firewall rule goes in first, so that the server is never
			// listening without one.
			firewallOK := firewall()
			if out, err := hidden(serverPath(), "install", "quiet"); err != nil {
				// The server program registers its own service.
				say("  The service could not be set up:\r\n  %s", strings.ReplaceAll(out, "\n", "\r\n  "))
				failed = true
			} else {
				host, _ := os.Hostname()
				pw, _ := os.ReadFile(filepath.Join(dir, "deskbridge-password.txt"))
				say("  Done. It is running now and starts with Windows.")
				say("")
				say("  To connect from the other PC, use:")
				say("    Computer:  %s", host)
				for _, ip := range localIPs() {
					say("          or:  %s", ip)
				}
				say("    Port:      %s", proto.DefaultPort)
				say("    Password:  %s", strings.TrimSpace(string(pw)))
				say("")
				if !firewallOK {
					say("  Other PCs cannot connect until the Windows Firewall problem above is fixed.")
					say("")
					failed = true
				}
			}
		}
	}

	if client {
		say("Installing the client...")
		if err := extract("client-"+arch+".exe", clientPath()); err != nil {
			say("  Failed: %v", err)
			failed = true
		} else {
			os.MkdirAll(filepath.Dir(shortcutPath()), 0755)
			script := `$s=(New-Object -ComObject WScript.Shell).CreateShortcut('` + shortcutPath() + `');` +
				`$s.TargetPath='` + clientPath() + `';$s.WorkingDirectory='` + dir + `';` +
				`$s.Description='Control another PC with Deskbridge';$s.Save()`
			if out, err := hidden("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script); err != nil {
				say("  Installed, but the Start menu shortcut could not be created: %s", out)
				say("  You can start it from: %s", clientPath())
			} else {
				say("  Done. Find Deskbridge in the Start menu, or press Open Deskbridge.")
			}
			say("")
		}
	}

	// Keep the manual, and a copy of this program for uninstalling later.
	extract("README.txt", filepath.Join(dir, "README.txt"))
	if self, err := os.Executable(); err == nil && !strings.EqualFold(self, setupPath()) {
		if data, err := os.ReadFile(self); err == nil {
			os.WriteFile(setupPath(), data, 0755)
		}
	}
	// List it under Settings > Apps > Installed apps.
	reg := func(name, kind, value string) {
		hidden("reg.exe", "add", uninstallKey, "/v", name, "/t", kind, "/d", value, "/f")
	}
	reg("DisplayName", "REG_SZ", "Deskbridge")
	reg("DisplayVersion", "REG_SZ", proto.Version)
	reg("Publisher", "REG_SZ", "Deskbridge")
	reg("InstallLocation", "REG_SZ", dir)
	reg("DisplayIcon", "REG_SZ", setupPath())
	reg("UninstallString", "REG_SZ", `"`+setupPath()+`" /uninstall`)
	reg("NoModify", "REG_DWORD", "1")
	reg("NoRepair", "REG_DWORD", "1")
	reg("EstimatedSize", "REG_DWORD", "20000")

	if failed {
		say("Setup finished with problems; see above.")
	} else {
		say("Setup finished. Version %s is installed.", proto.Version)
	}
	return !failed
}

func uninstall() {
	say("Removing Deskbridge...")
	if exists(serverPath()) {
		// Removes the service and the firewall rule.
		if out, err := hidden(serverPath(), "uninstall", "quiet"); err != nil {
			say("  The service could not be removed cleanly:\r\n  %s", strings.ReplaceAll(out, "\n", "\r\n  "))
		} else {
			say("  Server service and firewall rule removed.")
		}
	}
	removeFirewallRules()
	hidden("taskkill.exe", "/F", "/IM", "deskbridge-client.exe")
	os.Remove(shortcutPath())
	hidden("reg.exe", "delete", uninstallKey, "/f")
	for _, f := range []string{serverPath(), clientPath(), filepath.Join(installDir(), "README.txt")} {
		for try := 0; try < 20 && exists(f); try++ {
			if os.Remove(f) == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	say("  Programs, Start menu shortcut and the Installed apps entry removed.")
	say("")
	say("Deskbridge has been removed. The install folder is deleted when you close this window.")
	say("The client's saved settings (in your user profile) were left alone.")
}

// uninstalled is set once Deskbridge has been removed and not put back.
var uninstalled bool

// removeFolderLater starts a separate command that waits a few seconds and
// then deletes the install folder, by which time this program has exited.
func removeFolderLater() {
	cmd := exec.Command("cmd.exe")
	// The command line is given whole: cmd.exe has its own quoting rules.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000,
		CmdLine: `cmd.exe /C ping -n 4 127.0.0.1 >nul & rmdir /S /Q "` + installDir() + `"`}
	cmd.Dir = os.Getenv("SystemRoot")
	cmd.Start()
}

//go:build windows

package main

// Windows service support. The service itself runs in session 0, where there
// is no desktop, so its only job is to keep a "worker" copy of this program
// running as SYSTEM inside whichever session owns the physical console. The
// worker can then see and control the sign-in screen, the lock screen and
// admin approval (UAC) prompts as well as the normal desktop.

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

const (
	serviceName    = "Deskbridge"
	serviceDisplay = "Deskbridge remote desktop server"
	firewallRule   = "Deskbridge server"
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	pOpenSCManager          = advapi32.NewProc("OpenSCManagerW")
	pCreateService          = advapi32.NewProc("CreateServiceW")
	pOpenService            = advapi32.NewProc("OpenServiceW")
	pDeleteService          = advapi32.NewProc("DeleteService")
	pStartService           = advapi32.NewProc("StartServiceW")
	pControlService         = advapi32.NewProc("ControlService")
	pQueryServiceStatus     = advapi32.NewProc("QueryServiceStatus")
	pCloseServiceHandle     = advapi32.NewProc("CloseServiceHandle")
	pStartServiceDispatcher = advapi32.NewProc("StartServiceCtrlDispatcherW")
	pRegisterServiceHandler = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	pSetServiceStatus       = advapi32.NewProc("SetServiceStatus")
	pDuplicateTokenEx       = advapi32.NewProc("DuplicateTokenEx")
	pSetTokenInformation    = advapi32.NewProc("SetTokenInformation")

	pShellExecute = shell32.NewProc("ShellExecuteW")

	pActiveConsoleSession = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

const (
	svcStopped      = 1
	svcStartPending = 2
	svcStopPending  = 3
	svcRunning      = 4
)

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func installDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "Deskbridge")
}

// quiet is set when the setup program runs "install quiet" or "uninstall
// quiet": no prompts, no waiting for Enter, and the result as an exit code.
var (
	quiet     bool
	installOK bool
)

func pause() {
	if quiet {
		return
	}
	fmt.Println()
	fmt.Println("Press Enter to close this window.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

// openSCM returns a service manager handle, or relaunches this command
// elevated (and reports false) when the caller is not an administrator.
func openSCM(command string) (uintptr, bool) {
	scm, _, err := pOpenSCManager.Call(0, 0, 0xF003F)
	if scm != 0 {
		return scm, true
	}
	if err == syscall.ERROR_ACCESS_DENIED && !quiet && !(len(os.Args) > 2 && os.Args[2] == "elevated") {
		exe, _ := os.Executable()
		r, _, _ := pShellExecute.Call(0, uintptr(unsafe.Pointer(u16("runas"))), uintptr(unsafe.Pointer(u16(exe))),
			uintptr(unsafe.Pointer(u16(command+" elevated"))), 0, 1)
		if r <= 32 {
			fmt.Println("Administrator approval is needed and was not given.")
			pause()
		}
		return 0, false
	}
	fmt.Println("Could not open the Windows service manager:", err)
	pause()
	return 0, false
}

// removeService stops and deletes the service if it exists.
func removeService(scm uintptr) {
	svc, _, _ := pOpenService.Call(scm, uintptr(unsafe.Pointer(u16(serviceName))), 0xF01FF)
	if svc == 0 {
		return
	}
	var st serviceStatus
	pControlService.Call(svc, 1, uintptr(unsafe.Pointer(&st)))
	for i := 0; i < 40; i++ {
		if r, _, _ := pQueryServiceStatus.Call(svc, uintptr(unsafe.Pointer(&st))); r == 0 || st.CurrentState == svcStopped {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	pDeleteService.Call(svc)
	pCloseServiceHandle.Call(svc)
	time.Sleep(500 * time.Millisecond)
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func install() {
	scm, ok := openSCM("install")
	if !ok {
		return
	}
	defer pCloseServiceHandle.Call(scm)
	defer pause()

	fmt.Println("Installing the Deskbridge service...")
	removeService(scm) // also stops a previous version so its exe can be replaced

	// Live in Program Files so that only administrators can replace a
	// program that Windows runs with full system rights.
	dir := installDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Println("Could not create", dir+":", err)
		return
	}
	src, _ := os.Executable()
	dst := filepath.Join(dir, "deskbridge-server.exe")
	if !strings.EqualFold(src, dst) {
		var err error
		for i := 0; i < 20; i++ { // the old worker may take a moment to exit
			if err = copyFile(src, dst); err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			fmt.Println("Could not copy the program to", dir+":", err)
			return
		}
	}

	// Keep the password you already use, if there is one beside this exe.
	pwPath := filepath.Join(dir, passwordFile)
	if _, err := os.Stat(pwPath); err != nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(src), passwordFile)); err == nil && strings.TrimSpace(string(b)) != "" {
			os.WriteFile(pwPath, b, 0600)
		}
	}
	password := loadOrCreatePasswordAt(pwPath)
	// Only SYSTEM and administrators may read the password.
	if err := run("icacls", pwPath, "/inheritance:r", "/grant:r", "*S-1-5-18:F", "*S-1-5-32-544:F"); err != nil {
		fmt.Println("Warning: could not restrict who can read the password file:", err)
	}

	run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	if err := run("netsh", "advfirewall", "firewall", "add", "rule", "name="+firewallRule, "dir=in", "action=allow",
		"program="+dst, "protocol=TCP", "localport="+proto.DefaultPort, "profile=private,domain", "enable=yes"); err != nil {
		fmt.Println("Warning: could not add the Windows Firewall rule; you may need to allow port", proto.DefaultPort, "yourself:", err)
	}

	binPath := `"` + dst + `" service`
	var svc uintptr
	var err error
	for i := 0; i < 20; i++ { // a just-deleted service can linger briefly
		svc, _, err = pCreateService.Call(scm, uintptr(unsafe.Pointer(u16(serviceName))), uintptr(unsafe.Pointer(u16(serviceDisplay))),
			0xF01FF, 0x10, 2, 1, uintptr(unsafe.Pointer(u16(binPath))), 0, 0, 0, 0, 0)
		if svc != 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if svc == 0 {
		fmt.Println("Could not create the service:", err)
		return
	}
	defer pCloseServiceHandle.Call(svc)
	if r, _, err := pStartService.Call(svc, 0, 0); r == 0 {
		fmt.Println("The service was installed but could not be started:", err)
		return
	}

	host, _ := os.Hostname()
	fmt.Println()
	fmt.Println("Done. Deskbridge now starts with Windows, before anyone signs in.")
	fmt.Println()
	fmt.Println("  Connect to:", host)
	for _, ip := range localIPs() {
		fmt.Println("          or:", ip)
	}
	fmt.Println("  Port:      ", proto.DefaultPort)
	fmt.Println("  Password:  ", password)
	fmt.Println()
	installOK = true
	fmt.Println("Installed in:", dir)
	fmt.Println("If something misbehaves, see deskbridge.log in that folder.")
}

func uninstall() {
	scm, ok := openSCM("uninstall")
	if !ok {
		return
	}
	defer pCloseServiceHandle.Call(scm)
	defer pause()
	removeService(scm)
	run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	dir := installDir()
	if self, _ := os.Executable(); !strings.EqualFold(filepath.Dir(self), dir) {
		time.Sleep(time.Second)
		os.RemoveAll(dir)
	}
	installOK = true
	fmt.Println("The Deskbridge service has been removed.")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// logToFile sends log output to deskbridge.log beside the exe, since the
// service and its worker have no window to print to.
func logToFile() {
	exe, _ := os.Executable()
	path := filepath.Join(filepath.Dir(exe), "deskbridge.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		os.Remove(path)
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		log.SetOutput(f)
	}
}

// ---------- running as a service ----------

var (
	svcHandle uintptr
	svcStop   = make(chan struct{})
	svcOnce   sync.Once
)

func setServiceState(state uint32) {
	st := serviceStatus{ServiceType: 0x10, CurrentState: state, WaitHint: 5000}
	if state == svcRunning {
		st.ControlsAccepted = 0x1 | 0x4 // stop, shutdown
	}
	pSetServiceStatus.Call(svcHandle, uintptr(unsafe.Pointer(&st)))
}

func serviceCtrl(control, eventType, eventData, context uintptr) uintptr {
	switch control {
	case 1, 5: // stop, shutdown
		setServiceState(svcStopPending)
		svcOnce.Do(func() { close(svcStop) })
		return 0
	case 4: // interrogate
		return 0
	}
	return 120 // ERROR_CALL_NOT_IMPLEMENTED
}

func serviceMain(argc, argv uintptr) uintptr {
	svcHandle, _, _ = pRegisterServiceHandler.Call(uintptr(unsafe.Pointer(u16(serviceName))), syscall.NewCallback(serviceCtrl), 0)
	if svcHandle == 0 {
		return 0
	}
	setServiceState(svcRunning)
	log.Print("service started")
	manageWorker()
	log.Print("service stopped")
	setServiceState(svcStopped)
	return 0
}

func runService() {
	logToFile()
	table := [2]struct {
		name *uint16
		proc uintptr
	}{{u16(serviceName), syscall.NewCallback(serviceMain)}, {}}
	if r, _, err := pStartServiceDispatcher.Call(uintptr(unsafe.Pointer(&table[0]))); r == 0 {
		log.Print("not started by the Windows service manager: ", err)
	}
}

// manageWorker keeps one worker alive in the console session, following the
// console when it moves to another session (sign-out, switch user).
func manageWorker() {
	var proc syscall.Handle
	var procSession uint32
	kill := func() {
		if proc != 0 {
			syscall.TerminateProcess(proc, 0)
			syscall.CloseHandle(proc)
			proc = 0
		}
	}
	defer kill()
	lastErr := ""
	for {
		sid, _, _ := pActiveConsoleSession.Call()
		session := uint32(sid)
		if proc != 0 {
			if ev, _ := syscall.WaitForSingleObject(proc, 0); ev == 0 { // exited
				syscall.CloseHandle(proc)
				proc = 0
			} else if session != procSession {
				kill()
			}
		}
		if proc == 0 && session != 0xFFFFFFFF {
			h, err := spawnWorker(session)
			if err != nil {
				if err.Error() != lastErr {
					lastErr = err.Error()
					log.Printf("could not start worker in session %d: %v", session, err)
				}
			} else {
				proc, procSession, lastErr = h, session, ""
				log.Printf("worker started in session %d", session)
			}
		}
		select {
		case <-svcStop:
			return
		case <-time.After(time.Second):
		}
	}
}

// spawnWorker starts "deskbridge-server.exe worker" as SYSTEM in the given
// session by giving a copy of our own token that session's id.
func spawnWorker(session uint32) (syscall.Handle, error) {
	self, _ := syscall.GetCurrentProcess()
	var tok syscall.Token
	if err := syscall.OpenProcessToken(self, 0xF01FF, &tok); err != nil {
		return 0, fmt.Errorf("open token: %w", err)
	}
	defer tok.Close()
	var dup syscall.Token
	// MAXIMUM_ALLOWED, SecurityImpersonation, TokenPrimary
	if r, _, err := pDuplicateTokenEx.Call(uintptr(tok), 0x02000000, 0, 2, 1, uintptr(unsafe.Pointer(&dup))); r == 0 {
		return 0, fmt.Errorf("duplicate token: %w", err)
	}
	defer dup.Close()
	// TokenSessionId = 12
	if r, _, err := pSetTokenInformation.Call(uintptr(dup), 12, uintptr(unsafe.Pointer(&session)), 4); r == 0 {
		return 0, fmt.Errorf("set token session: %w", err)
	}
	exe, _ := os.Executable()
	si := syscall.StartupInfo{Desktop: u16(`winsta0\default`)}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	const createNoWindow = 0x08000000
	if err := syscall.CreateProcessAsUser(dup, nil, u16(`"`+exe+`" worker`), nil, nil, false, createNoWindow,
		nil, u16(filepath.Dir(exe)), &si, &pi); err != nil {
		return 0, fmt.Errorf("create process: %w", err)
	}
	syscall.CloseHandle(pi.Thread)
	return pi.Process, nil
}

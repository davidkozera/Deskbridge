//go:build windows

package main

// Things done on the client's request that cannot be done by sending keys.
//
// Windows deliberately ignores Ctrl+Alt+Del and Win+L when they are typed by
// a program, so those are done another way; the rest (sign out, restart,
// shut down) are here because they belong in the same menu.

import (
	"log"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

var (
	pWTSLogoffSession  = wtsapi32.NewProc("WTSLogoffSession")
	pRegGetValue       = advapi32.NewProc("RegGetValueW")
	pRegSetKeyValue    = advapi32.NewProc("RegSetKeyValueW")
	pRegDeleteKeyValue = advapi32.NewProc("RegDeleteKeyValueW")
)

func tell(s *session, text string) {
	s.c.Send(proto.MsgNotice, true, append([]byte{proto.NoticeGeneral}, text...))
}

// typeKeys presses the given keys (by scan code) in order and releases them
// in reverse. It must run on the input thread.
func typeKeys(scans ...uint16) {
	press := func(scan uint16, up bool) {
		in := keybdInput{typ: 1, scan: scan, flags: 0x0008} // by scan code
		if up {
			in.flags |= 0x0002
		}
		pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	}
	for _, sc := range scans {
		press(sc, false)
	}
	for i := len(scans) - 1; i >= 0; i-- {
		press(scans[i], true)
	}
}

// doAction runs on the input thread.
func doAction(s *session, action byte) {
	switch action {
	case proto.ActionTaskManager:
		typeKeys(0x1D, 0x2A, 0x01) // Ctrl+Shift+Esc

	case proto.ActionCtrlAltDel:
		if !workerMode {
			tell(s, "Ctrl+Alt+Del can only be sent when Deskbridge is installed on the remote PC (not started by hand).")
			return
		}
		go func() {
			if err := sendCtrlAltDel(); err != nil {
				log.Print("Ctrl+Alt+Del: ", err)
				tell(s, "Ctrl+Alt+Del could not be sent: "+err.Error())
			}
		}()

	case proto.ActionLock:
		go func() {
			runtime.LockOSThread() // a fresh thread, on the normal desktop
			if r, _, err := pLockWorkStation.Call(); r == 0 {
				tell(s, "The remote PC could not be locked: "+err.Error())
			}
		}()

	case proto.ActionSignOut:
		if workerMode {
			// We run as SYSTEM, so "sign me out" would not mean the person
			// at the PC: end their session by number instead.
			var session uint32
			pProcessIdToSessionId.Call(uintptr(syscall.Getpid()), uintptr(unsafe.Pointer(&session)))
			if r, _, err := pWTSLogoffSession.Call(0, uintptr(session), 0); r == 0 {
				tell(s, "Signing out failed: "+err.Error())
			}
		} else if r, _, err := pExitWindowsEx.Call(0, 0); r == 0 {
			tell(s, "Signing out failed: "+err.Error())
		}

	case proto.ActionRestart, proto.ActionShutDown:
		flag, word := "/r", "restarting"
		if action == proto.ActionShutDown {
			flag, word = "/s", "shutting down"
		}
		cmd := exec.Command("shutdown.exe", flag, "/t", "0")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		if out, err := cmd.CombinedOutput(); err != nil {
			tell(s, "The remote PC refused: "+string(out))
		} else {
			tell(s, "The remote PC is "+word+".")
		}
	}
}

// sendCtrlAltDel asks Windows to act as if Ctrl+Alt+Del had been pressed.
// Windows only honours that from a service when a policy allows it, so the
// policy is switched on for the moment of the call and then put back as it
// was.
func sendCtrlAltDel() error {
	sendSAS := syscall.NewLazyDLL("sas.dll").NewProc("SendSAS")
	if err := sendSAS.Find(); err != nil {
		return err
	}
	const hklm = uintptr(syscall.HKEY_LOCAL_MACHINE)
	key, _ := syscall.UTF16PtrFromString(`SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`)
	name, _ := syscall.UTF16PtrFromString("SoftwareSASGeneration")
	kp, np := uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(name))

	var old, size uint32 = 0, 4
	r, _, _ := pRegGetValue.Call(hklm, kp, np, 0x10 /* a number */, 0, uintptr(unsafe.Pointer(&old)), uintptr(unsafe.Pointer(&size)))
	had := r == 0
	if !had {
		old = 0
	}
	if old&1 == 0 { // bit 1: services may do this
		allowed := old | 1
		if r, _, _ := pRegSetKeyValue.Call(hklm, kp, np, 4 /* a number */, uintptr(unsafe.Pointer(&allowed)), 4); r != 0 {
			return syscall.Errno(r)
		}
		defer func() {
			time.Sleep(500 * time.Millisecond) // let Windows act on it first
			if had {
				pRegSetKeyValue.Call(hklm, kp, np, 4, uintptr(unsafe.Pointer(&old)), 4)
			} else {
				pRegDeleteKeyValue.Call(hklm, kp, np)
			}
		}()
	}
	sendSAS.Call(0) // 0: sent by a service, not by the signed-in person
	return nil
}

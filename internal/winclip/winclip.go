//go:build windows

// Package winclip reads and writes text and file lists on the Windows
// clipboard, and watches it for changes.
package winclip

import (
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	pOpenClipboard    = user32.NewProc("OpenClipboard")
	pCloseClipboard   = user32.NewProc("CloseClipboard")
	pEmptyClipboard   = user32.NewProc("EmptyClipboard")
	pGetClipboardData = user32.NewProc("GetClipboardData")
	pSetClipboardData = user32.NewProc("SetClipboardData")
	pIsFormatAvail    = user32.NewProc("IsClipboardFormatAvailable")
	pEnumFormats      = user32.NewProc("EnumClipboardFormats")
	pGetFormatName    = user32.NewProc("GetClipboardFormatNameW")
	pRegisterFormat   = user32.NewProc("RegisterClipboardFormatW")
	pGetSequence      = user32.NewProc("GetClipboardSequenceNumber")
	pAddListener      = user32.NewProc("AddClipboardFormatListener")
	pRegisterClassEx  = user32.NewProc("RegisterClassExW")
	pCreateWindowEx   = user32.NewProc("CreateWindowExW")
	pDefWindowProc    = user32.NewProc("DefWindowProcW")
	pGetMessage       = user32.NewProc("GetMessageW")
	pDispatchMessage  = user32.NewProc("DispatchMessageW")
	pPostMessage      = user32.NewProc("PostMessageW")

	pGlobalAlloc     = kernel32.NewProc("GlobalAlloc")
	pGlobalFree      = kernel32.NewProc("GlobalFree")
	pGlobalLock      = kernel32.NewProc("GlobalLock")
	pGlobalUnlock    = kernel32.NewProc("GlobalUnlock")
	pGlobalSize      = kernel32.NewProc("GlobalSize")
	pGetModuleHandle = kernel32.NewProc("GetModuleHandleW")

	pDragQueryFile = shell32.NewProc("DragQueryFileW")
)

const (
	cfUnicodeText = 13
	cfHDrop       = 15

	// Update is WM_CLIPBOARDUPDATE, sent to a window registered with Listen.
	Update = 0x031D
)

func open(hwnd uintptr) bool {
	for i := 0; i < 15; i++ { // another program may hold it for a moment
		if r, _, _ := pOpenClipboard.Call(hwnd); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Listen asks Windows to send Update to hwnd whenever the clipboard changes.
func Listen(hwnd uintptr) { pAddListener.Call(hwnd) }

// Sequence is a counter Windows bumps on every clipboard change.
func Sequence() uint32 {
	r, _, _ := pGetSequence.Call()
	return uint32(r)
}

func formatName(f uintptr) string {
	var buf [256]uint16
	n, _, _ := pGetFormatName.Call(f, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// ReadAll returns the copied files if there are any; otherwise every
// transferable format on the clipboard (text, pictures, rich text...).
// Call it on the thread that owns hwnd.
// droppedFiles lists the paths in a file-drop handle (an HDROP).
func droppedFiles(h uintptr) (files []string) {
	n, _, _ := pDragQueryFile.Call(h, 0xFFFFFFFF, 0, 0)
	for i := uintptr(0); i < n && i < 10000; i++ {
		l, _, _ := pDragQueryFile.Call(h, i, 0, 0)
		if l == 0 || l > 32767 {
			continue
		}
		buf := make([]uint16, l+1)
		pDragQueryFile.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), l+1)
		files = append(files, syscall.UTF16ToString(buf))
	}
	return files
}

func ReadAll(hwnd uintptr) (formats []proto.ClipFormat, files []string) {
	if !open(hwnd) {
		return nil, nil
	}
	defer pCloseClipboard.Call()
	if r, _, _ := pIsFormatAvail.Call(cfHDrop); r != 0 {
		if h, _, _ := pGetClipboardData.Call(cfHDrop); h != 0 {
			files = droppedFiles(h)
			if len(files) > 0 {
				return nil, files
			}
		}
	}
	// List first, fetch second: fetching can make programs produce data.
	var ids []uintptr
	for f := uintptr(0); len(ids) < 200; {
		f, _, _ = pEnumFormats.Call(f)
		if f == 0 {
			break
		}
		ids = append(ids, f)
	}
	total := 0
	for _, f := range ids {
		cf := proto.ClipFormat{ID: uint32(f)}
		if f >= 0xC000 { // registered by a program: identified by name
			cf.ID, cf.Name = 0, formatName(f)
		}
		if !proto.ClipTransferable(cf.ID, cf.Name) {
			continue
		}
		h, _, _ := pGetClipboardData.Call(f)
		if h == 0 {
			continue
		}
		size, _, _ := pGlobalSize.Call(h)
		if size == 0 || size > proto.ClipMaxFormat || total+int(size) > proto.ClipMaxBlob {
			continue
		}
		p, _, _ := pGlobalLock.Call(h)
		if p == 0 {
			continue
		}
		cf.Data = append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(p)), size)...)
		pGlobalUnlock.Call(h)
		if f == cfUnicodeText { // cut at the terminator; the block may be longer
			for i := 0; i+1 < len(cf.Data); i += 2 {
				if cf.Data[i] == 0 && cf.Data[i+1] == 0 {
					cf.Data = cf.Data[:i+2]
					break
				}
			}
			if len(cf.Data) <= 2 {
				continue
			}
		}
		total += len(cf.Data)
		formats = append(formats, cf)
	}
	return formats, nil
}

// SetFormats replaces the clipboard with the given formats. Call it on the
// thread that owns hwnd.
func SetFormats(hwnd uintptr, formats []proto.ClipFormat) bool {
	if len(formats) == 0 || !open(hwnd) {
		return false
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	ok := false
	for _, f := range formats {
		if len(f.Data) == 0 || !proto.ClipTransferable(f.ID, f.Name) {
			continue
		}
		id := uintptr(f.ID)
		if f.Name != "" {
			name, err := syscall.UTF16PtrFromString(f.Name)
			if err != nil {
				continue
			}
			if id, _, _ = pRegisterFormat.Call(uintptr(unsafe.Pointer(name))); id == 0 {
				continue
			}
		}
		ok = setData(id, f.Data) || ok
	}
	return ok
}

// setData adds one block to the clipboard, which must be open.
func setData(format uintptr, data []byte) bool {
	h, _, _ := pGlobalAlloc.Call(0x0002, uintptr(len(data))) // GMEM_MOVEABLE
	if h == 0 {
		return false
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return false
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(p)), len(data)), data)
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(format, h); r == 0 {
		pGlobalFree.Call(h) // on success the clipboard owns the memory
		return false
	}
	return true
}

// put replaces the clipboard contents with one block of data.
func put(hwnd uintptr, format uintptr, data []byte) bool {
	if len(data) == 0 || !open(hwnd) {
		return false
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	return setData(format, data)
}

func utf16Bytes(u []uint16) []byte {
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2], b[i*2+1] = byte(v), byte(v>>8)
	}
	return b
}

// SetFiles puts a list of files and folders on the clipboard, as if they had
// been copied in File Explorer. Call it on the thread that owns hwnd.
func SetFiles(hwnd uintptr, paths []string) bool {
	// DROPFILES header: offset of the list (20), a point, a flag, "wide".
	data := []byte{20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0}
	count := 0
	for _, p := range paths {
		u, err := syscall.UTF16FromString(p)
		if err != nil || len(u) < 2 {
			continue
		}
		data = append(data, utf16Bytes(u)...)
		count++
	}
	if count == 0 {
		return false
	}
	data = append(data, 0, 0) // the list ends with an empty string
	return put(hwnd, cfHDrop, data)
}

// ---------- running work on the window's thread ----------

var tasks = make(chan func(), 256)

// Post arranges for f to run on the thread that owns hwnd; that window's
// procedure must call RunTasks when it receives message.
func Post(hwnd, message uintptr, f func()) {
	select {
	case tasks <- f:
		pPostMessage.Call(hwnd, message, 0, 0)
	default:
	}
}

// RunTasks runs everything queued by Post.
func RunTasks() {
	for {
		select {
		case f := <-tasks:
			f()
		default:
			return
		}
	}
}

// ---------- window plumbing ----------

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

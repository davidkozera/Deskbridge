//go:build windows

package main

// Exporting the saved connections to a file and importing them again: a
// backup, and a way to carry them to another PC. The file is a CSV (see
// internal/connfile) and holds the passwords as plain text, because the
// form they are kept in here can only be read by this Windows account on
// this PC.

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"deskbridge/internal/connfile"
	"deskbridge/internal/update"
)

const idMore = 118

var (
	comdlg32         = syscall.NewLazyDLL("comdlg32.dll")
	pGetSaveFileName = comdlg32.NewProc("GetSaveFileNameW")
	pGetOpenFileName = comdlg32.NewProc("GetOpenFileNameW")

	hMore uintptr
)

// openFileName is Windows' OPENFILENAMEW.
type openFileName struct {
	structSize     uint32
	owner          uintptr
	instance       uintptr
	filter         *uint16
	customFilter   *uint16
	maxCustFilter  uint32
	filterIndex    uint32
	file           *uint16
	maxFile        uint32
	fileTitle      *uint16
	maxFileTitle   uint32
	initialDir     *uint16
	title          *uint16
	flags          uint32
	fileOffset     uint16
	fileExtension  uint16
	defExt         *uint16
	custData       uintptr
	hook           uintptr
	templateName   *uint16
	reserved       uintptr
	reservedNumber uint32
	flagsEx        uint32
}

// chooseFile shows Windows' "Save as" or "Open" box for a CSV file and
// returns the path chosen, or "" if it was cancelled.
func chooseFile(save bool, title, suggested string) string {
	const (
		overwritePrompt, hideReadOnly, noChangeDir = 0x2, 0x4, 0x8
		pathMustExist, fileMustExist, explorer     = 0x800, 0x1000, 0x80000
	)
	filter := utf16.Encode([]rune("CSV files (*.csv)\x00*.csv\x00All files (*.*)\x00*.*\x00\x00"))
	file := make([]uint16, 1024)
	copy(file, utf16.Encode([]rune(suggested)))
	ofn := openFileName{
		owner: hMain, filter: &filter[0], filterIndex: 1, file: &file[0], maxFile: uint32(len(file)),
		title: u16ptr(title), defExt: u16ptr("csv"), flags: explorer | hideReadOnly | noChangeDir | pathMustExist,
	}
	ofn.structSize = uint32(unsafe.Sizeof(ofn))
	proc := pGetOpenFileName
	if save {
		ofn.flags |= overwritePrompt
		proc = pGetSaveFileName
	} else {
		ofn.flags |= fileMustExist
	}
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&ofn))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(file)
}

// moreMenu shows the menu under the "More" button.
func moreMenu() {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	const exportID, importID = 1, 2
	pAppendMenu.Call(menu, 0, exportID, uintptr(unsafe.Pointer(u16ptr("Export connections..."))))
	pAppendMenu.Call(menu, 0, importID, uintptr(unsafe.Pointer(u16ptr("Import connections..."))))
	addInternetMenu(menu)
	addChannelMenu(menu)
	var rc rect
	pGetWindowRect.Call(hMore, uintptr(unsafe.Pointer(&rc)))
	// TPM_RETURNCMD | TPM_NONOTIFY: just tell us what was picked.
	switch choice, _, _ := pTrackPopupMenu.Call(menu, 0x0100|0x0080, uintptr(rc.left), uintptr(rc.bottom), 0, hMain, 0); choice {
	case exportID:
		exportConnections()
	case importID:
		importConnections()
	case relayUseID:
		useCopiedRelayCode()
	case relayOffID:
		turnRelayOff()
	case channelMenuBase + update.Alpha, channelMenuBase + update.Beta, channelMenuBase + update.Mainline:
		channelPicked(int(choice) - channelMenuBase)
	}
}

// exportConnections writes every saved connection to a CSV file of the
// person's choosing.
func exportConnections() {
	if len(computers) == 0 {
		setStatus("There are no saved connections to export yet. Connect to a PC, or fill in the boxes and click Save.")
		return
	}
	const okCancel, warningIcon, ok = 0x1, 0x30, 1
	warning := "The file will hold the remembered passwords of your saved connections as plain text.\n\n" +
		"Anyone who can open it can connect to those PCs, so keep it somewhere only you can reach, " +
		"and delete it once it is no longer needed."
	if r, _, _ := pMessageBoxW.Call(hMain, uintptr(unsafe.Pointer(u16ptr(warning))),
		uintptr(unsafe.Pointer(u16ptr("Export connections"))), okCancel|warningIcon); r != ok {
		return
	}
	path := chooseFile(true, "Export connections", "Deskbridge connections.csv")
	if path == "" {
		return
	}
	var list []connfile.Connection
	withPassword := 0
	for _, c := range computers {
		row := connfile.Connection{Name: c.name, Host: c.host, Port: c.port, Width: c.res.w, Height: c.res.h, Display: c.display}
		if o := c.opts; c.hasOpts {
			row.HasSettings, row.FPS, row.Quality = true, o.fps, o.quality
			row.Sound, row.AllowSleep, row.CopyPaste, row.Video, row.FastCapture = o.audio, o.sleep, o.clip, o.video, o.fast
		}
		if pw := dpapi(pCryptUnprotectData, c.password); pw != nil {
			row.Password = string(pw)
			withPassword++
		}
		list = append(list, row)
	}
	if err := os.WriteFile(path, connfile.Encode(list), 0600); err != nil {
		setStatus("The connections could not be exported: " + err.Error())
		return
	}
	note := ""
	if withPassword < len(list) {
		note = fmt.Sprintf(" %d of them had no remembered password, so the file has none for those.", len(list)-withPassword)
	}
	setStatus(fmt.Sprintf("Exported %d %s to:\r\n%s\r\n\r\nThe file holds passwords as plain text. Keep it safe.%s",
		len(list), plural(len(list), "connection"), path, note))
}

// importConnections adds the connections in a CSV file to the saved list.
// One whose name is already in the list replaces that entry.
func importConnections() {
	path := chooseFile(false, "Import connections", "")
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err == nil && info.Size() > 1<<20 {
		setStatus("That file is too large to be a list of connections.")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		setStatus("The file could not be read: " + err.Error())
		return
	}
	list, skipped, err := connfile.Decode(data)
	if err != nil {
		setStatus("Nothing was imported from that file: " + err.Error() + ".")
		return
	}
	added, replaced, full := 0, 0, 0
	for _, row := range list {
		c := computer{name: row.Name, host: row.Host, port: row.Port, res: resolution{row.Width, row.Height}, display: row.Display}
		if row.Password != "" {
			c.password = dpapi(pCryptProtectData, []byte(row.Password))
		}
		if row.HasSettings {
			c.hasOpts = true
			c.opts = options{fps: row.FPS, quality: row.Quality, audio: row.Sound, sleep: row.AllowSleep,
				clip: row.CopyPaste, video: row.Video, fast: row.FastCapture}
		}
		if i := findComputer(c.name); i >= 0 {
			if strings.EqualFold(computers[i].host, c.host) {
				c.modes = computers[i].modes // still the same PC's display
			}
			computers[i] = c
			replaced++
		} else if len(computers) >= maxComputers {
			full++
		} else {
			computers = append(computers, c)
			added++
		}
	}
	fillComputers()
	if i := currentProfile(); i >= 0 {
		loadProfile(i) // the connection in the boxes may have been replaced
	}
	saveSettings()
	checkServers()
	s := fmt.Sprintf("Imported %d %s: %d new, %d replacing saved ones of the same name.", added+replaced, plural(added+replaced, "connection"), added, replaced)
	if skipped > 0 {
		s += fmt.Sprintf("\r\n\r\n%d %s left out: no computer given, a repeated name, or more than the file may hold.", skipped, plural(skipped, "row was", "rows were"))
	}
	if full > 0 {
		s += fmt.Sprintf("\r\n\r\n%d more did not fit: the list holds %d connections.", full, maxComputers)
	}
	setStatus(s + "\r\n\r\nPick one from the Connection name list to use it.")
}

// plural picks the word for a count: plural(n, "row") or, for an irregular
// one, plural(n, "row was", "rows were").
func plural(n int, forms ...string) string {
	switch {
	case n == 1:
		return forms[0]
	case len(forms) > 1:
		return forms[1]
	}
	return forms[0] + "s"
}

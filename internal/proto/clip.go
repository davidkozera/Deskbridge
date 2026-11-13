package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Copied files are not sent when they are copied. The PC they were copied on
// only announces a list; the other PC puts "virtual" files on its clipboard
// and asks for the bytes, a window at a time, when something pastes them.
const (
	MsgClipList      = 37 // id u32, count u32, entries (see EncodeList); count 0 withdraws the offer
	MsgClipFetch     = 38 // id u32, index u32, offset u64, length u32
	MsgClipFetchData = 39 // id u32, index u32, offset u64, status u8, bytes
	MsgClipFetchStop = 40 // id u32, index u32: the reader is done with this file
	MsgClipState     = 41 // u8: used only between the service worker and its clipboard helper
)

const (
	ClipMaxFiles = 20000
	clipChunk    = 256 << 10
	FetchWindow  = 4 << 20 // most bytes asked for at once, so little is ever buffered

	FetchMore       = 0 // more of this window follows
	FetchWindowDone = 1 // the requested window is complete
	FetchEOF        = 2 // the file ends here
	FetchError      = 3
)

var ErrClipTooBig = errors.New("too many items to copy across")

// ListEntry is one copied file or folder.
type ListEntry struct {
	Index   uint32 // position in the sender's list; used to ask for contents
	Rel     string // path relative to what was copied, "/" separated
	Dir     bool
	Size    int64
	ModTime int64  // Unix seconds
	abs     string // sender only
}

// PlanList walks the copied paths. Folders come before their contents.
func PlanList(paths []string) ([]ListEntry, error) {
	var entries []ListEntry
	for _, p := range paths {
		base := filepath.Base(p)
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil // unreadable items are left out
			}
			if d.Type()&fs.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil // links, devices and the like are left out
			}
			rel, rerr := filepath.Rel(p, path)
			info, ierr := d.Info()
			if rerr != nil || ierr != nil {
				return nil
			}
			e := ListEntry{Index: uint32(len(entries)), Rel: filepath.ToSlash(filepath.Join(base, rel)), Dir: d.IsDir(),
				ModTime: info.ModTime().Unix(), abs: path}
			if !e.Dir {
				e.Size = info.Size()
			}
			entries = append(entries, e)
			if len(entries) > ClipMaxFiles {
				return ErrClipTooBig
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func EncodeList(id uint32, entries []ListEntry) []byte {
	b := binary.LittleEndian.AppendUint32(nil, id)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		flag := byte(0)
		if e.Dir {
			flag = 1
		}
		b = append(b, flag)
		b = binary.LittleEndian.AppendUint64(b, uint64(e.Size))
		b = binary.LittleEndian.AppendUint64(b, uint64(e.ModTime))
		b = binary.LittleEndian.AppendUint16(b, uint16(len(e.Rel)))
		b = append(b, e.Rel...)
	}
	return b
}

// safeRel turns a sender-supplied relative path into components that cannot
// escape the folder being pasted into or name anything Windows treats
// specially.
func safeRel(rel string) ([]string, error) {
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || len(parts) > 64 || len(rel) > 2000 {
		return nil, errors.New("bad path")
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `\/:*?"<>|`) ||
			strings.HasSuffix(p, ".") || strings.HasSuffix(p, " ") {
			return nil, fmt.Errorf("bad path component %q", p)
		}
		for _, c := range p {
			if c < 32 {
				return nil, errors.New("bad path character")
			}
		}
		stem, _, _ := strings.Cut(strings.ToUpper(p), ".")
		switch stem {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return nil, errors.New("reserved name")
		}
	}
	return parts, nil
}

// DecodeList reads a list and keeps only entries that are safe to hand to
// File Explorer: sane names, short enough for a file descriptor, and inside
// a folder that was itself accepted.
func DecodeList(p []byte) (id uint32, entries []ListEntry, err error) {
	bad := errors.New("malformed file list")
	if len(p) < 8 {
		return 0, nil, bad
	}
	id = binary.LittleEndian.Uint32(p)
	n := binary.LittleEndian.Uint32(p[4:])
	if n > ClipMaxFiles {
		return 0, nil, ErrClipTooBig
	}
	p = p[8:]
	dirs := map[string]bool{}
	for i := uint32(0); i < n; i++ {
		if len(p) < 19 {
			return 0, nil, bad
		}
		e := ListEntry{Index: i, Dir: p[0] == 1, Size: int64(binary.LittleEndian.Uint64(p[1:])), ModTime: int64(binary.LittleEndian.Uint64(p[9:]))}
		nl := int(binary.LittleEndian.Uint16(p[17:]))
		if len(p) < 19+nl {
			return 0, nil, bad
		}
		e.Rel = string(p[19 : 19+nl])
		p = p[19+nl:]
		parts, perr := safeRel(e.Rel)
		if perr != nil || e.Size < 0 || len(utf16.Encode([]rune(e.Rel))) > 259 {
			continue
		}
		if len(parts) > 1 && !dirs[strings.ToLower(strings.Join(parts[:len(parts)-1], "/"))] {
			continue
		}
		if e.Dir {
			e.Size = 0
			dirs[strings.ToLower(e.Rel)] = true
		}
		entries = append(entries, e)
	}
	return id, entries, nil
}

// ---------- asking for and serving file contents ----------

type FetchReq struct {
	ID, Index uint32
	Offset    int64
	Length    uint32
}

func EncodeFetch(r FetchReq) []byte {
	b := binary.LittleEndian.AppendUint32(nil, r.ID)
	b = binary.LittleEndian.AppendUint32(b, r.Index)
	b = binary.LittleEndian.AppendUint64(b, uint64(r.Offset))
	return binary.LittleEndian.AppendUint32(b, r.Length)
}

func DecodeFetch(p []byte) (FetchReq, bool) {
	if len(p) < 20 {
		return FetchReq{}, false
	}
	r := FetchReq{ID: binary.LittleEndian.Uint32(p), Index: binary.LittleEndian.Uint32(p[4:]),
		Offset: int64(binary.LittleEndian.Uint64(p[8:])), Length: binary.LittleEndian.Uint32(p[16:])}
	return r, r.Offset >= 0
}

func encodeFetchData(r FetchReq, offset int64, status byte, data []byte) []byte {
	b := make([]byte, 0, 17+len(data))
	b = binary.LittleEndian.AppendUint32(b, r.ID)
	b = binary.LittleEndian.AppendUint32(b, r.Index)
	b = binary.LittleEndian.AppendUint64(b, uint64(offset))
	b = append(b, status)
	return append(b, data...)
}

func DecodeFetchData(p []byte) (id, index uint32, offset int64, status byte, data []byte, ok bool) {
	if len(p) < 17 {
		return
	}
	return binary.LittleEndian.Uint32(p), binary.LittleEndian.Uint32(p[4:]), int64(binary.LittleEndian.Uint64(p[8:])), p[16], p[17:], true
}

// FileServer hands out the contents of the files most recently copied on
// this PC. It only ever reads files from that list, chosen by position;
// the other PC never supplies a path.
type FileServer struct {
	mu      sync.Mutex
	active  bool
	id      uint32
	entries []ListEntry
	f       *os.File
	fIndex  uint32
}

func (s *FileServer) closeFile() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

// Offer replaces the files on offer.
func (s *FileServer) Offer(id uint32, entries []ListEntry) {
	s.mu.Lock()
	s.closeFile()
	s.active, s.id, s.entries = true, id, entries
	s.mu.Unlock()
}

// Clear withdraws the offer. It reports whether there was one.
func (s *FileServer) Clear() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.active
	s.closeFile()
	s.active, s.entries = false, nil
	return was
}

// CloseFile lets go of the file last read, so it is not held open.
func (s *FileServer) CloseFile() {
	s.mu.Lock()
	s.closeFile()
	s.mu.Unlock()
}

func (s *FileServer) open(r FetchReq) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || r.ID != s.id || int(r.Index) >= len(s.entries) || s.entries[r.Index].Dir {
		return nil
	}
	if s.f == nil || s.fIndex != r.Index {
		s.closeFile()
		f, err := os.Open(s.entries[r.Index].abs)
		if err != nil {
			return nil
		}
		s.f, s.fIndex = f, r.Index
	}
	return s.f
}

// Serve answers one request, sending the window in pieces.
func (s *FileServer) Serve(r FetchReq, send func(t byte, p []byte) error, cancelled func() bool) {
	f := s.open(r)
	if f == nil {
		send(MsgClipFetchData, encodeFetchData(r, r.Offset, FetchError, nil))
		return
	}
	remaining, off := int64(min(r.Length, FetchWindow)), r.Offset
	buf := make([]byte, clipChunk)
	for !cancelled() {
		n := int(min(int64(len(buf)), remaining))
		m, err := f.ReadAt(buf[:n], off)
		status := byte(FetchMore)
		switch {
		case err == io.EOF:
			status = FetchEOF
		case err != nil:
			status, m = FetchError, 0
		case remaining == int64(m):
			status = FetchWindowDone
		}
		if send(MsgClipFetchData, encodeFetchData(r, off, status, buf[:m])) != nil || status != FetchMore {
			return
		}
		off += int64(m)
		remaining -= int64(m)
	}
}

// FetchStream is the reading end: it looks like an ordinary file being read
// front to back, and pulls the bytes across as they are needed.
type FetchStream struct {
	ID, Index uint32
	Size      int64
	request   func(offset int64, length uint32) error
	timeout   time.Duration

	mu      sync.Mutex
	notify  chan struct{}
	pos     int64  // next byte Read will return
	buf     []byte // bytes received and not yet read, starting at pos
	recvEnd int64  // offset just past the last byte received
	reqEnd  int64  // offset just past the last byte asked for
	eof     bool
	aborted bool
}

var ErrFetchAborted = errors.New("the copy was cancelled or the other PC stopped answering")

func NewFetchStream(id, index uint32, size int64, request func(offset int64, length uint32) error) *FetchStream {
	return &FetchStream{ID: id, Index: index, Size: size, request: request, timeout: 30 * time.Second, notify: make(chan struct{}, 1)}
}

// NewEmptyStream is a stream with nothing in it (used for folders).
func NewEmptyStream() *FetchStream {
	f := NewFetchStream(0, 0, 0, func(int64, uint32) error { return nil })
	f.eof = true
	return f
}

func (f *FetchStream) wake() {
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// Abort makes pending and future reads fail.
func (f *FetchStream) Abort() {
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
	f.wake()
}

// Deliver hands the stream bytes that arrived. Anything that is not exactly
// the next expected piece (stale data from before a seek, say) is ignored.
func (f *FetchStream) Deliver(offset int64, status byte, data []byte) {
	f.mu.Lock()
	if f.aborted || offset != f.recvEnd || offset+int64(len(data)) > f.reqEnd {
		f.mu.Unlock()
		return
	}
	if len(f.buf) == 0 {
		f.buf = data
	} else {
		f.buf = append(f.buf, data...)
	}
	f.recvEnd += int64(len(data))
	switch status {
	case FetchWindowDone:
		f.reqEnd = f.recvEnd
	case FetchEOF:
		f.eof = true
	case FetchError:
		f.aborted = true
	}
	f.mu.Unlock()
	f.wake()
}

// Read blocks until at least one byte is available, the file ends (io.EOF)
// or the transfer fails.
func (f *FetchStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		f.mu.Lock()
		if f.aborted {
			f.mu.Unlock()
			return 0, ErrFetchAborted
		}
		// Ask for the next window once the last one has fully arrived and
		// there is room, so at most two windows are ever held in memory.
		ask, askAt := false, int64(0)
		if !f.eof && f.recvEnd == f.reqEnd && len(f.buf) < FetchWindow {
			ask, askAt = true, f.recvEnd
			f.reqEnd += FetchWindow
		}
		n := copy(p, f.buf)
		f.buf = f.buf[n:]
		f.pos += int64(n)
		eof := f.eof
		f.mu.Unlock()
		if ask {
			if err := f.request(askAt, FetchWindow); err != nil {
				f.Abort()
				return n, ErrFetchAborted
			}
		}
		if n > 0 {
			return n, nil
		}
		if eof {
			return 0, io.EOF
		}
		select {
		case <-f.notify:
		case <-time.After(f.timeout):
			f.Abort()
			return 0, ErrFetchAborted
		}
	}
}

// Seek moves the read position. Moving away from the current position drops
// what was buffered and restarts fetching from the new place.
func (f *FetchStream) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekCurrent:
		offset += f.pos
	case io.SeekEnd:
		offset += f.Size
	case io.SeekStart:
	default:
		return f.pos, errors.New("bad whence")
	}
	if offset < 0 {
		return f.pos, errors.New("negative position")
	}
	if offset != f.pos {
		f.pos, f.buf, f.recvEnd, f.reqEnd, f.eof = offset, nil, offset, offset, false
	}
	return f.pos, nil
}

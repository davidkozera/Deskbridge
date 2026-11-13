//go:build windows

package winclip

import (
	"errors"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"deskbridge/internal/proto"
)

// Sender delivers one message to the other PC, reliably and in order.
type Sender func(t byte, p []byte) error

// Sync keeps this PC's clipboard and the other PC's in step. Text, pictures
// and similar content are sent as soon as they are copied. Files are only
// announced; their contents cross when something pastes them.
//
// It runs its own thread with a hidden window: that thread owns all access
// to the clipboard, and may sit waiting for file data during a paste without
// freezing anything else.
type Sync struct {
	Enabled func() bool  // is sharing switched on right now?
	Status  func(string) // optional: tell the person what happened

	hwnd   uintptr
	cur    atomic.Pointer[Peer]
	gen    atomic.Uint32 // bumped on every change; cancels older sends
	setSeq atomic.Uint32 // clipboard sequence number after our own last write

	server  proto.FileServer // files copied here, on offer to the other PC
	fetches chan fetchJob

	mu          sync.Mutex
	remotePrint proto.ClipPrint // what the other PC last put here, so it is not sent straight back
	streams     map[*proto.FetchStream]bool

	current uintptr // our data object while it is on the clipboard (clipboard thread only)
}

type fetchJob struct {
	p   *Peer
	req proto.FetchReq
}

// Peer is one connection's side of the exchange.
type Peer struct {
	s    *Sync
	send Sender
	blob proto.BlobReceiver // used only by the goroutine calling Remote
}

const taskMessage = 0x8000 + 1

// Start creates the clipboard thread. It reports false if that failed, in
// which case clipboard sharing is simply unavailable.
func (s *Sync) Start() bool {
	s.streams = map[*proto.FetchStream]bool{}
	s.fetches = make(chan fetchJob, 64)
	ready := make(chan uintptr, 1)
	go s.thread(ready)
	s.hwnd = <-ready
	if s.hwnd == 0 {
		return false
	}
	go func() { // serves the other PC's requests for file contents, one at a time
		for j := range s.fetches {
			s.server.Serve(j.req, j.p.send, func() bool { return s.cur.Load() != j.p })
		}
	}()
	return true
}

func (s *Sync) thread(ready chan<- uintptr) {
	runtime.LockOSThread()
	pOleInitialize.Call(0)
	oleSetup()
	inst, _, _ := pGetModuleHandle.Call(0)
	name, _ := syscall.UTF16PtrFromString("DeskbridgeClipboard")
	proc := func(h, message, wParam, lParam uintptr) uintptr {
		switch message {
		case Update:
			s.localChanged()
			return 0
		case taskMessage:
			RunTasks()
			return 0
		}
		r, _, _ := pDefWindowProc.Call(h, message, wParam, lParam)
		return r
	}
	wc := wndClassEx{wndProc: syscall.NewCallback(proc), instance: inst, className: name}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	// Parent HWND_MESSAGE (-3) makes it an invisible message-only window.
	hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, inst, 0)
	if hwnd != 0 {
		Listen(hwnd)
	}
	ready <- hwnd
	if hwnd == 0 {
		return
	}
	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// onThread runs f on the clipboard thread.
func (s *Sync) onThread(f func()) { Post(s.hwnd, taskMessage, f) }

// Connect makes send the destination for local copies.
func (s *Sync) Connect(send Sender) *Peer {
	p := &Peer{s: s, send: send}
	s.cur.Store(p)
	return p
}

// Close ends the connection's sharing: transfers in progress fail, and files
// from the other PC that can no longer be fetched leave the clipboard.
func (p *Peer) Close() {
	s := p.s
	s.cur.CompareAndSwap(p, nil)
	s.gen.Add(1)
	p.blob.Abort()
	s.server.Clear()
	s.abortStreams()
	s.onThread(s.withdraw)
}

// Shutdown takes our virtual files off the clipboard before the program
// exits, so nothing is left pointing at a process that is gone.
func (s *Sync) Shutdown() {
	if s.hwnd == 0 {
		return
	}
	done := make(chan struct{})
	s.onThread(func() { s.withdraw(); close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func (s *Sync) say(msg string) {
	if s.Status != nil {
		s.Status(msg)
	}
}

func (s *Sync) abortStreams() {
	s.mu.Lock()
	for f := range s.streams {
		f.Abort()
	}
	s.mu.Unlock()
}

// withdraw removes our data object from the clipboard if it is still there.
// Clipboard thread only.
func (s *Sync) withdraw() {
	if s.current != 0 {
		if r, _, _ := pOleIsCurrentClipboard.Call(s.current); r == sOK {
			pOleSetClipboard.Call(0)
			s.setSeq.Store(Sequence())
		}
		s.current = 0
	}
}

// objectGone is called when COM lets go of a data object for good.
func (s *Sync) objectGone(d *dataObject) {
	if s.current == d.ptr() {
		s.current = 0
	}
}

// openStream makes the stream File Explorer reads one file's contents from.
func (s *Sync) openStream(d *dataObject, e proto.ListEntry) *streamObject {
	if e.Dir {
		return newStreamObject(s, d.peer, proto.NewEmptyStream(), e.ModTime)
	}
	p, id, index := d.peer, d.id, e.Index
	f := proto.NewFetchStream(id, index, e.Size, func(offset int64, length uint32) error {
		if s.cur.Load() != p {
			return errors.New("disconnected")
		}
		return p.send(proto.MsgClipFetch, proto.EncodeFetch(proto.FetchReq{ID: id, Index: index, Offset: offset, Length: length}))
	})
	s.mu.Lock()
	s.streams[f] = true
	s.mu.Unlock()
	if s.cur.Load() != p {
		f.Abort()
	}
	return newStreamObject(s, p, f, e.ModTime)
}

// streamGone is called when the reader has released a stream: finished, or
// cancelled from Explorer's progress window.
func (s *Sync) streamGone(st *streamObject) {
	st.f.Abort()
	s.mu.Lock()
	known := s.streams[st.f]
	delete(s.streams, st.f)
	s.mu.Unlock()
	if known && s.cur.Load() == st.peer {
		id, index := st.f.ID, st.f.Index
		go st.peer.send(proto.MsgClipFetchStop, proto.EncodeFetch(proto.FetchReq{ID: id, Index: index})[:8])
	}
}

// localChanged reacts to something being copied on this PC. Clipboard
// thread only.
func (s *Sync) localChanged() {
	p := s.cur.Load()
	if p == nil || !s.Enabled() {
		return
	}
	if seq := s.setSeq.Load(); seq != 0 && seq == Sequence() {
		return // this is our own write being reported back
	}
	formats, files := ReadAll(s.hwnd)
	s.mu.Lock()
	echo := len(files) == 0 && s.remotePrint.Matches(formats)
	if !echo {
		s.remotePrint = nil // this PC's clipboard is its own again
	}
	s.mu.Unlock()
	if echo {
		return
	}
	gen := s.gen.Add(1)
	cancelled := func() bool { return s.gen.Load() != gen || s.cur.Load() != p }
	switch {
	case len(files) > 0:
		go func() {
			entries, err := proto.PlanList(files)
			if cancelled() {
				return
			}
			if err != nil || len(entries) == 0 {
				if errors.Is(err, proto.ErrClipTooBig) {
					s.say("The copied files cannot be pasted on the other PC: more than " + strconv.Itoa(proto.ClipMaxFiles) + " items.")
				}
				if s.server.Clear() {
					p.send(proto.MsgClipList, proto.EncodeList(gen, nil))
				}
				return
			}
			s.server.Offer(gen, entries)
			p.send(proto.MsgClipList, proto.EncodeList(gen, entries))
		}()
	case len(formats) > 0:
		s.server.Clear() // the receiving side replaces its clipboard anyway
		go func() {
			// Compressing a large picture takes a moment; not on this thread.
			proto.SendBlob(gen, proto.EncodeFormats(formats), p.send, cancelled)
		}()
	default:
		// Nothing we can share is on the clipboard now. If the other PC is
		// still showing files from here, take them back.
		if s.server.Clear() {
			go p.send(proto.MsgClipList, proto.EncodeList(gen, nil))
		}
	}
}

// Remote handles a clipboard message from the other PC. Call it from the one
// goroutine that reads the connection; it never waits on the network.
func (p *Peer) Remote(t byte, payload []byte) {
	s := p.s
	if !s.Enabled() || s.hwnd == 0 {
		return
	}
	switch t {
	case proto.MsgClipBlobBegin, proto.MsgClipBlobData, proto.MsgClipBlobEnd:
		blob := p.blob.Handle(t, payload)
		if blob == nil {
			return
		}
		formats, err := proto.DecodeFormats(blob)
		if err != nil || len(formats) == 0 {
			return
		}
		s.gen.Add(1)
		s.server.Clear()
		s.mu.Lock()
		s.remotePrint = proto.Fingerprint(formats)
		s.mu.Unlock()
		s.onThread(func() {
			if SetFormats(s.hwnd, formats) {
				s.setSeq.Store(Sequence())
			}
		})

	case proto.MsgClipList:
		id, entries, err := proto.DecodeList(payload)
		if err != nil {
			return
		}
		s.gen.Add(1)
		s.server.Clear()
		s.abortStreams()
		s.mu.Lock()
		s.remotePrint = nil
		s.mu.Unlock()
		s.onThread(func() {
			s.withdraw()
			if len(entries) == 0 || s.cur.Load() != p {
				return
			}
			d := newDataObject(s, p, id, entries)
			var hr uintptr
			for try := 0; try < 10; try++ { // another program may hold the clipboard for a moment
				if hr, _, _ = pOleSetClipboard.Call(d.ptr()); int32(hr) >= 0 {
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
			if int32(hr) >= 0 {
				s.current = d.ptr()
				s.setSeq.Store(Sequence())
			}
			doRelease(d.ptr()) // the clipboard holds its own reference now
		})

	case proto.MsgClipFetch:
		if req, ok := proto.DecodeFetch(payload); ok {
			select {
			case s.fetches <- fetchJob{p, req}:
			default: // absurd backlog; the reader will time out
			}
		}

	case proto.MsgClipFetchData:
		id, index, offset, status, data, ok := proto.DecodeFetchData(payload)
		if !ok {
			return
		}
		s.mu.Lock()
		for f := range s.streams {
			if f.ID == id && f.Index == index {
				f.Deliver(offset, status, data)
			}
		}
		s.mu.Unlock()

	case proto.MsgClipFetchStop:
		s.server.CloseFile()
	}
}

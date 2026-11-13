//go:build windows

package winclip

// Virtual files on the clipboard, the way Remote Desktop does it.
//
// Instead of real files, the clipboard holds a COM "data object" that offers
// two things: a list of file names and sizes (FileGroupDescriptorW), and each
// file's contents as a stream (FileContents). When the person pastes in File
// Explorer, Explorer shows its usual copy progress window and reads the
// streams; each read pulls bytes from the other PC.
//
// The objects here are COM objects written by hand: a block of memory whose
// first word points to a table of functions. Go keeps them alive in a
// registry for as long as COM holds references.

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"deskbridge/internal/proto"
)

var (
	ole32 = syscall.NewLazyDLL("ole32.dll")

	pOleInitialize         = ole32.NewProc("OleInitialize")
	pOleSetClipboard       = ole32.NewProc("OleSetClipboard")
	pOleIsCurrentClipboard = ole32.NewProc("OleIsCurrentClipboard")
	pSHCreateStdEnumFmtEtc = shell32.NewProc("SHCreateStdEnumFmtEtc")
)

type guid struct {
	d1 uint32
	d2 uint16
	d3 uint16
	d4 [8]byte
}

var (
	iidUnknown    = guid{0x00000000, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidDataObject = guid{0x0000010E, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidStream     = guid{0x0000000C, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidSeqStream  = guid{0x0C733A30, 0x2A1C, 0x11CE, [8]byte{0xAD, 0xE5, 0x00, 0xAA, 0x00, 0x44, 0x77, 0x3D}}
	// IDataObjectAsyncCapability: lets Explorer do the paste on a
	// background thread so its window stays responsive.
	iidAsync = guid{0x3D8B0590, 0xF691, 0x11D2, [8]byte{0x8E, 0xA9, 0x00, 0x60, 0x97, 0xDF, 0x5B, 0xD4}}
)

const (
	sOK                 = 0
	sFalse              = 1
	eNotImpl            = 0x80004001
	eNoInterface        = 0x80004002
	ePointer            = 0x80004003
	eOutOfMemory        = 0x8007000E
	dvEFormatEtc        = 0x80040064
	dvELindex           = 0x80040068
	oleEAdviseNotSupp   = 0x80040003
	stgEInvalidFunction = 0x80030001
	stgEAccessDenied    = 0x80030005
	stgEReadFault       = 0x8003001E

	tymedHGlobal = 1
	tymedIStream = 4
)

type formatEtc struct {
	cf     uint16
	ptd    uintptr
	aspect uint32
	lindex int32
	tymed  uint32
}

type stgMedium struct {
	tymed uint32
	u     uintptr
	unk   uintptr
}

var (
	oleOnce                                sync.Once
	cfDescriptor, cfContents, cfDropEffect uint16
	dataVtbl                               [12]uintptr
	asyncVtbl                              [8]uintptr
	streamVtbl                             [14]uintptr
	objMu                                  sync.Mutex
	dataObjs                               = map[uintptr]*dataObject{}
	streamObjs                             = map[uintptr]*streamObject{}
)

func registerFormat(name string) uint16 {
	p, _ := syscall.UTF16PtrFromString(name)
	r, _, _ := pRegisterFormat.Call(uintptr(unsafe.Pointer(p)))
	return uint16(r)
}

func oleSetup() {
	oleOnce.Do(func() {
		cfDescriptor = registerFormat("FileGroupDescriptorW")
		cfContents = registerFormat("FileContents")
		cfDropEffect = registerFormat("Preferred DropEffect")
		cb := syscall.NewCallback
		dataVtbl = [12]uintptr{
			cb(doQueryInterface), cb(doAddRef), cb(doRelease),
			cb(doGetData), cb(doGetDataHere), cb(doQueryGetData), cb(doGetCanonicalFormatEtc),
			cb(doSetData), cb(doEnumFormatEtc), cb(doDAdvise), cb(doDUnadvise), cb(doEnumDAdvise),
		}
		asyncVtbl = [8]uintptr{
			cb(doQueryInterface), cb(doAddRef), cb(doRelease),
			cb(asSetAsyncMode), cb(asGetAsyncMode), cb(asStartOperation), cb(asInOperation), cb(asEndOperation),
		}
		streamVtbl = [14]uintptr{
			cb(stQueryInterface), cb(stAddRef), cb(stRelease),
			cb(stRead), cb(stWrite), cb(stSeek), cb(stSetSize), cb(stCopyTo), cb(stCommit), cb(stRevert),
			cb(stLockRegion), cb(stLockRegion), cb(stStat), cb(stClone),
		}
	})
}

func putPtr(at uintptr, v uintptr) {
	if at != 0 {
		*(*uintptr)(unsafe.Pointer(at)) = v
	}
}

func putU32(at uintptr, v uint32) {
	if at != 0 {
		*(*uint32)(unsafe.Pointer(at)) = v
	}
}

func isGUID(at uintptr, g *guid) bool { return at != 0 && *(*guid)(unsafe.Pointer(at)) == *g }

// globalFrom copies data into a movable memory block, the form the
// clipboard and data objects pass data around in.
func globalFrom(data []byte) uintptr {
	h, _, _ := pGlobalAlloc.Call(0x0002, uintptr(len(data)))
	if h == 0 {
		return 0
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(p)), len(data)), data)
	pGlobalUnlock.Call(h)
	return h
}

// ---------- the data object ----------

type dataObject struct {
	vtbl      uintptr // IDataObject
	vtblAsync uintptr // IDataObjectAsyncCapability; its "this" is the address of this field
	refs      int32
	s         *Sync
	peer      *Peer
	id        uint32
	entries   []proto.ListEntry
	async     uint32
	inOp      uint32
}

func (d *dataObject) ptr() uintptr      { return uintptr(unsafe.Pointer(d)) }
func (d *dataObject) asyncPtr() uintptr { return uintptr(unsafe.Pointer(&d.vtblAsync)) }

func newDataObject(s *Sync, peer *Peer, id uint32, entries []proto.ListEntry) *dataObject {
	oleSetup()
	d := &dataObject{refs: 1, s: s, peer: peer, id: id, entries: entries, async: 1}
	d.vtbl = uintptr(unsafe.Pointer(&dataVtbl[0]))
	d.vtblAsync = uintptr(unsafe.Pointer(&asyncVtbl[0]))
	objMu.Lock()
	dataObjs[d.ptr()] = d
	dataObjs[d.asyncPtr()] = d
	objMu.Unlock()
	return d
}

func getData(this uintptr) *dataObject {
	objMu.Lock()
	defer objMu.Unlock()
	return dataObjs[this]
}

func doQueryInterface(this, riid, ppv uintptr) uintptr {
	d := getData(this)
	if d == nil || ppv == 0 {
		return ePointer
	}
	switch {
	case isGUID(riid, &iidUnknown), isGUID(riid, &iidDataObject):
		putPtr(ppv, d.ptr())
	case isGUID(riid, &iidAsync):
		putPtr(ppv, d.asyncPtr())
	default:
		putPtr(ppv, 0)
		return eNoInterface
	}
	atomic.AddInt32(&d.refs, 1)
	return sOK
}

func doAddRef(this uintptr) uintptr {
	if d := getData(this); d != nil {
		return uintptr(atomic.AddInt32(&d.refs, 1))
	}
	return 0
}

func doRelease(this uintptr) uintptr {
	d := getData(this)
	if d == nil {
		return 0
	}
	n := atomic.AddInt32(&d.refs, -1)
	if n == 0 {
		objMu.Lock()
		delete(dataObjs, d.ptr())
		delete(dataObjs, d.asyncPtr())
		objMu.Unlock()
		d.s.objectGone(d)
	}
	return uintptr(n)
}

// descriptor builds the FILEGROUPDESCRIPTORW block: a count followed by one
// 592-byte FILEDESCRIPTORW per item.
func (d *dataObject) descriptor() []byte {
	const (
		recSize      = 592
		fdAttributes = 0x00000004
		fdWritesTime = 0x00000020
		fdFileSize   = 0x00000040
		fdProgressUI = 0x00004000 // ask Explorer for its progress window
		fdUnicode    = 0x80000000
	)
	b := make([]byte, 4+recSize*len(d.entries))
	le := func(off int, v uint32) {
		b[off], b[off+1], b[off+2], b[off+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	le(0, uint32(len(d.entries)))
	for i, e := range d.entries {
		o := 4 + recSize*i
		flags := uint32(fdAttributes | fdProgressUI | fdUnicode)
		if e.Dir {
			le(o+36, 0x10) // FILE_ATTRIBUTE_DIRECTORY
		} else {
			flags |= fdFileSize | fdWritesTime
			le(o+36, 0x80) // FILE_ATTRIBUTE_NORMAL
			ft := uint64(e.ModTime)*10000000 + 116444736000000000
			if e.ModTime <= 0 {
				flags &^= fdWritesTime
				ft = 0
			}
			le(o+56, uint32(ft))
			le(o+60, uint32(ft>>32))
			le(o+64, uint32(uint64(e.Size)>>32))
			le(o+68, uint32(e.Size))
		}
		le(o, flags)
		name := utf16.Encode([]rune(strings.ReplaceAll(e.Rel, "/", `\`)))
		for j, u := range name {
			if j >= 259 {
				break
			}
			b[o+72+j*2], b[o+72+j*2+1] = byte(u), byte(u>>8)
		}
	}
	return b
}

func doGetData(this, pfe, pmed uintptr) uintptr {
	d := getData(this)
	if d == nil || pfe == 0 || pmed == 0 {
		return ePointer
	}
	fe := *(*formatEtc)(unsafe.Pointer(pfe))
	med := (*stgMedium)(unsafe.Pointer(pmed))
	*med = stgMedium{}
	var block []byte
	switch {
	case fe.cf == cfContents && fe.tymed&tymedIStream != 0:
		if fe.lindex < 0 || int(fe.lindex) >= len(d.entries) {
			return dvELindex
		}
		st := d.s.openStream(d, d.entries[fe.lindex])
		med.tymed, med.u = tymedIStream, st.ptr()
		return sOK
	case fe.cf == cfDescriptor && fe.tymed&tymedHGlobal != 0:
		block = d.descriptor()
	case fe.cf == cfDropEffect && fe.tymed&tymedHGlobal != 0:
		block = []byte{1, 0, 0, 0} // DROPEFFECT_COPY
	default:
		return dvEFormatEtc
	}
	h := globalFrom(block)
	if h == 0 {
		return eOutOfMemory
	}
	med.tymed, med.u = tymedHGlobal, h
	return sOK
}

func doQueryGetData(this, pfe uintptr) uintptr {
	if getData(this) == nil || pfe == 0 {
		return ePointer
	}
	fe := *(*formatEtc)(unsafe.Pointer(pfe))
	switch {
	case fe.cf == cfContents && fe.tymed&tymedIStream != 0,
		(fe.cf == cfDescriptor || fe.cf == cfDropEffect) && fe.tymed&tymedHGlobal != 0:
		return sOK
	}
	return dvEFormatEtc
}

func doEnumFormatEtc(this, direction, ppenum uintptr) uintptr {
	if getData(this) == nil || ppenum == 0 {
		return ePointer
	}
	putPtr(ppenum, 0)
	if direction != 1 { // DATADIR_GET
		return eNotImpl
	}
	formats := [3]formatEtc{
		{cf: cfDescriptor, aspect: 1, lindex: -1, tymed: tymedHGlobal},
		{cf: cfContents, aspect: 1, lindex: -1, tymed: tymedIStream},
		{cf: cfDropEffect, aspect: 1, lindex: -1, tymed: tymedHGlobal},
	}
	hr, _, _ := pSHCreateStdEnumFmtEtc.Call(3, uintptr(unsafe.Pointer(&formats[0])), ppenum)
	return hr
}

func doGetDataHere(this, pfe, pmed uintptr) uintptr { return eNotImpl }
func doGetCanonicalFormatEtc(this, in, out uintptr) uintptr {
	if out != 0 {
		(*formatEtc)(unsafe.Pointer(out)).ptd = 0
	}
	return eNotImpl
}
func doSetData(this, pfe, pmed, release uintptr) uintptr    { return eNotImpl }
func doDAdvise(this, pfe, advf, sink, conn uintptr) uintptr { return oleEAdviseNotSupp }
func doDUnadvise(this, conn uintptr) uintptr                { return oleEAdviseNotSupp }
func doEnumDAdvise(this, ppenum uintptr) uintptr            { return oleEAdviseNotSupp }

func asSetAsyncMode(this, on uintptr) uintptr {
	if d := getData(this); d != nil {
		v := uint32(0)
		if on != 0 {
			v = 1
		}
		atomic.StoreUint32(&d.async, v)
	}
	return sOK
}

func asGetAsyncMode(this, out uintptr) uintptr {
	d := getData(this)
	if d == nil || out == 0 {
		return ePointer
	}
	putU32(out, atomic.LoadUint32(&d.async))
	return sOK
}

func asStartOperation(this, bindCtx uintptr) uintptr {
	if d := getData(this); d != nil {
		atomic.StoreUint32(&d.inOp, 1)
	}
	return sOK
}

func asInOperation(this, out uintptr) uintptr {
	d := getData(this)
	if d == nil || out == 0 {
		return ePointer
	}
	putU32(out, atomic.LoadUint32(&d.inOp))
	return sOK
}

func asEndOperation(this, hr, bindCtx, effects uintptr) uintptr {
	if d := getData(this); d != nil {
		atomic.StoreUint32(&d.inOp, 0)
	}
	return sOK
}

// ---------- the file-contents stream ----------

type streamObject struct {
	vtbl uintptr // IStream
	refs int32
	s    *Sync
	peer *Peer
	f    *proto.FetchStream
	mod  int64
}

func (st *streamObject) ptr() uintptr { return uintptr(unsafe.Pointer(st)) }

func newStreamObject(s *Sync, peer *Peer, f *proto.FetchStream, mod int64) *streamObject {
	st := &streamObject{refs: 1, s: s, peer: peer, f: f, mod: mod}
	st.vtbl = uintptr(unsafe.Pointer(&streamVtbl[0]))
	objMu.Lock()
	streamObjs[st.ptr()] = st
	objMu.Unlock()
	return st
}

func getStream(this uintptr) *streamObject {
	objMu.Lock()
	defer objMu.Unlock()
	return streamObjs[this]
}

func stQueryInterface(this, riid, ppv uintptr) uintptr {
	st := getStream(this)
	if st == nil || ppv == 0 {
		return ePointer
	}
	if isGUID(riid, &iidUnknown) || isGUID(riid, &iidStream) || isGUID(riid, &iidSeqStream) {
		putPtr(ppv, st.ptr())
		atomic.AddInt32(&st.refs, 1)
		return sOK
	}
	putPtr(ppv, 0)
	return eNoInterface
}

func stAddRef(this uintptr) uintptr {
	if st := getStream(this); st != nil {
		return uintptr(atomic.AddInt32(&st.refs, 1))
	}
	return 0
}

func stRelease(this uintptr) uintptr {
	st := getStream(this)
	if st == nil {
		return 0
	}
	n := atomic.AddInt32(&st.refs, -1)
	if n == 0 {
		objMu.Lock()
		delete(streamObjs, st.ptr())
		objMu.Unlock()
		st.s.streamGone(st)
	}
	return uintptr(n)
}

// stRead fills the caller's buffer, waiting for bytes from the other PC as
// needed. A short count with S_FALSE means the file ended.
func stRead(this, pv, cb, pcbRead uintptr) uintptr {
	st := getStream(this)
	if st == nil || (pv == 0 && cb != 0) {
		return ePointer
	}
	putU32(pcbRead, 0)
	want := int(uint32(cb))
	if want == 0 {
		return sOK
	}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(pv)), want)
	total := 0
	for total < want {
		n, err := st.f.Read(buf[total:])
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			putU32(pcbRead, uint32(total))
			return stgEReadFault
		}
	}
	putU32(pcbRead, uint32(total))
	if total < want {
		return sFalse
	}
	return sOK
}

func stSeek(this, move, origin, newPos uintptr) uintptr {
	st := getStream(this)
	if st == nil {
		return ePointer
	}
	whence, ok := map[uintptr]int{0: io.SeekStart, 1: io.SeekCurrent, 2: io.SeekEnd}[origin&0xFFFFFFFF]
	if !ok {
		return stgEInvalidFunction
	}
	pos, err := st.f.Seek(int64(move), whence)
	if err != nil {
		return stgEInvalidFunction
	}
	if newPos != 0 {
		*(*uint64)(unsafe.Pointer(newPos)) = uint64(pos)
	}
	return sOK
}

// stStat fills a STATSTG (80 bytes): just "a stream of this size".
func stStat(this, pstat, flag uintptr) uintptr {
	st := getStream(this)
	if st == nil || pstat == 0 {
		return ePointer
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(pstat)), 80)
	clear(b)
	*(*uint32)(unsafe.Pointer(pstat + 8)) = 2 // STGTY_STREAM
	*(*uint64)(unsafe.Pointer(pstat + 16)) = uint64(st.f.Size)
	if st.mod > 0 {
		*(*uint64)(unsafe.Pointer(pstat + 24)) = uint64(st.mod)*10000000 + 116444736000000000
	}
	return sOK
}

func stWrite(this, pv, cb, written uintptr) uintptr           { return stgEAccessDenied }
func stSetSize(this, size uintptr) uintptr                    { return stgEAccessDenied }
func stCopyTo(this, dst, cb, read, written uintptr) uintptr   { return eNotImpl }
func stCommit(this, flags uintptr) uintptr                    { return sOK }
func stRevert(this uintptr) uintptr                           { return eNotImpl }
func stLockRegion(this, offset, cb, lockType uintptr) uintptr { return stgEInvalidFunction }
func stClone(this, ppstm uintptr) uintptr {
	putPtr(ppstm, 0)
	return eNotImpl
}

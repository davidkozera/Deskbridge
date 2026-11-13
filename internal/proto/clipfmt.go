package proto

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"strconv"
	"strings"
)

// Everything on the clipboard other than a list of files (text, rich text,
// pictures, spreadsheet cells...) travels as one compressed "blob" holding
// each clipboard format's raw bytes.
const (
	MsgClipBlobBegin = 30 // id u32, blob length u32
	MsgClipBlobData  = 35 // id u32, bytes
	MsgClipBlobEnd   = 36 // id u32, ok u8
)

const (
	ClipMaxBlob   = 320 << 20 // bytes in one copy, before compression
	ClipMaxFormat = 256 << 20 // bytes in one clipboard format
)

// IsClipMsg reports whether t is one of the clipboard messages.
func IsClipMsg(t byte) bool { return t >= MsgClipBlobBegin && t <= MsgClipState }

// ClipFormat is one clipboard format. Built-in Windows formats are known by
// number (ID); formats that programs register are known by Name, with ID 0,
// because their numbers differ from one PC to the next.
type ClipFormat struct {
	ID   uint32
	Name string
	Data []byte
}

// Built-in formats that are plain blocks of memory and mean the same on any
// PC. Left out on purpose: formats Windows derives from these by itself
// (plain ANSI text, bitmap handles, the V5 bitmap), handles that only make
// sense inside one PC (bitmaps, metafiles, palettes) and file lists, which
// have their own path.
var clipStandard = map[uint32]bool{
	13: true, // Unicode text
	8:  true, // device-independent bitmap: pictures and screenshots
	4:  true, // SYLK
	5:  true, // DIF
	6:  true, // TIFF
	11: true, // RIFF
	12: true, // WAVE
}

// Registered formats that refer to live objects or local paths on the PC
// they came from, and so cannot work on another one.
var clipDenied = map[string]bool{
	"dataobject": true, "ole private data": true, "object descriptor": true,
	"link source": true, "link source descriptor": true, "embed source": true,
	"embedded object": true, "native": true, "ownerlink": true, "objectlink": true,
	"shell idlist array": true, "shell object offsets": true, "preferred dropeffect": true,
	"filegroupdescriptor": true, "filegroupdescriptorw": true, "filecontents": true,
	"filename": true, "filenamew": true, "dropdescription": true, "asyncflag": true,
	"hyperlink": true, "moniker": true,
}

// ClipTransferable reports whether a format may be sent or applied. Both
// ends check, so neither relies on the other behaving.
func ClipTransferable(id uint32, name string) bool {
	if name == "" {
		return clipStandard[id]
	}
	return id == 0 && len(name) <= 200 && !clipDenied[strings.ToLower(name)]
}

func EncodeFormats(formats []ClipFormat) []byte {
	var out bytes.Buffer
	fw, _ := flate.NewWriter(&out, flate.BestSpeed)
	var hdr []byte
	hdr = binary.LittleEndian.AppendUint16(hdr, uint16(len(formats)))
	fw.Write(hdr)
	for _, f := range formats {
		hdr = binary.LittleEndian.AppendUint32(hdr[:0], f.ID)
		hdr = binary.LittleEndian.AppendUint16(hdr, uint16(len(f.Name)))
		hdr = append(hdr, f.Name...)
		hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(f.Data)))
		fw.Write(hdr)
		fw.Write(f.Data)
	}
	fw.Close()
	return out.Bytes()
}

// DecodeFormats unpacks a blob, dropping anything that is not transferable.
func DecodeFormats(blob []byte) ([]ClipFormat, error) {
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(blob)), ClipMaxBlob+(1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > ClipMaxBlob+(1<<20) {
		return nil, ErrClipTooBig
	}
	bad := errors.New("malformed clipboard data")
	if len(raw) < 2 {
		return nil, bad
	}
	n := int(binary.LittleEndian.Uint16(raw))
	raw = raw[2:]
	var out []ClipFormat
	for i := 0; i < n; i++ {
		if len(raw) < 6 {
			return nil, bad
		}
		f := ClipFormat{ID: binary.LittleEndian.Uint32(raw)}
		nl := int(binary.LittleEndian.Uint16(raw[4:]))
		raw = raw[6:]
		if len(raw) < nl+4 {
			return nil, bad
		}
		f.Name = string(raw[:nl])
		dl := int(binary.LittleEndian.Uint32(raw[nl:]))
		raw = raw[nl+4:]
		if dl < 0 || len(raw) < dl {
			return nil, bad
		}
		f.Data = raw[:dl]
		raw = raw[dl:]
		if len(f.Data) > 0 && ClipTransferable(f.ID, f.Name) {
			out = append(out, f)
		}
	}
	return out, nil
}

// SendBlob streams an encoded blob in pieces small enough to share the
// connection with the screen and input.
func SendBlob(id uint32, blob []byte, send func(t byte, p []byte) error, cancelled func() bool) error {
	u32 := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	if err := send(MsgClipBlobBegin, append(u32(id), u32(uint32(len(blob)))...)); err != nil {
		return err
	}
	for len(blob) > 0 {
		if cancelled() {
			send(MsgClipBlobEnd, append(u32(id), 0))
			return errors.New("cancelled")
		}
		n := min(len(blob), clipChunk)
		if err := send(MsgClipBlobData, append(u32(id), blob[:n]...)); err != nil {
			return err
		}
		blob = blob[n:]
	}
	return send(MsgClipBlobEnd, append(u32(id), 1))
}

// BlobReceiver reassembles what SendBlob sends.
type BlobReceiver struct {
	active bool
	id     uint32
	want   int
	buf    []byte
}

func (r *BlobReceiver) Abort() { r.active, r.buf = false, nil }

// Handle returns the complete blob once its last piece has arrived.
func (r *BlobReceiver) Handle(t byte, p []byte) []byte {
	if len(p) < 4 {
		return nil
	}
	id := binary.LittleEndian.Uint32(p)
	if t == MsgClipBlobBegin {
		r.Abort()
		if len(p) < 8 {
			return nil
		}
		want := int(binary.LittleEndian.Uint32(p[4:]))
		if want <= 0 || want > ClipMaxBlob+(1<<20) {
			return nil
		}
		r.active, r.id, r.want, r.buf = true, id, want, make([]byte, 0, want)
		return nil
	}
	if !r.active || id != r.id {
		return nil
	}
	switch t {
	case MsgClipBlobData:
		if len(r.buf)+len(p)-4 > r.want {
			r.Abort()
			return nil
		}
		r.buf = append(r.buf, p[4:]...)
	case MsgClipBlobEnd:
		buf, ok := r.buf, len(p) >= 5 && p[4] == 1 && len(r.buf) == r.want
		r.Abort()
		if ok {
			return buf
		}
	}
	return nil
}

// ClipPrint is a compact description of clipboard contents we placed
// ourselves, used to recognise them when Windows reports the change back so
// that they are not sent straight back to where they came from.
type ClipPrint map[string][2]uint64 // format -> {length, hash of the start}

const printSpan = 64 << 10

func printKey(f ClipFormat) string { return strconv.Itoa(int(f.ID)) + "|" + strings.ToLower(f.Name) }

func printHash(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

func Fingerprint(formats []ClipFormat) ClipPrint {
	p := ClipPrint{}
	for _, f := range formats {
		p[printKey(f)] = [2]uint64{uint64(len(f.Data)), printHash(f.Data[:min(len(f.Data), printSpan)])}
	}
	return p
}

// Matches reports whether formats is what the fingerprint was taken from.
// Windows may round a block's size up a little, so a slightly longer block
// with the same beginning still counts.
func (p ClipPrint) Matches(formats []ClipFormat) bool {
	if len(p) == 0 || len(formats) < len(p) {
		return false
	}
	found := 0
	for _, f := range formats {
		want, ok := p[printKey(f)]
		if !ok {
			continue
		}
		n := uint64(len(f.Data))
		if n < want[0] || n-want[0] > 4096 || printHash(f.Data[:min(want[0], printSpan)]) != want[1] {
			return false
		}
		found++
	}
	return found == len(p)
}

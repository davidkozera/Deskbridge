package proto

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestClipList(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "one.txt"), []byte("hello"), 0644)
	os.MkdirAll(filepath.Join(src, "folder", "sub", "empty"), 0755)
	os.WriteFile(filepath.Join(src, "folder", "sub", "big.bin"), make([]byte, 1000), 0644)
	entries, err := PlanList([]string{filepath.Join(src, "one.txt"), filepath.Join(src, "folder")})
	if err != nil || len(entries) != 5 {
		t.Fatal(len(entries), err)
	}
	id, got, err := DecodeList(EncodeList(9, entries))
	if err != nil || id != 9 || len(got) != 5 {
		t.Fatal(id, len(got), err)
	}
	want := []string{"one.txt", "folder", "folder/sub", "folder/sub/big.bin", "folder/sub/empty"}
	for i, e := range got {
		if e.Rel != want[i] || e.Index != uint32(i) || e.Dir != (i == 1 || i == 2 || i == 4) {
			t.Fatal(i, e)
		}
	}
	if got[0].Size != 5 || got[3].Size != 1000 || got[0].ModTime == 0 {
		t.Fatal(got)
	}

	// Hostile or unusable names are dropped, along with anything inside a
	// dropped folder; the rest keep their original positions.
	long := string(bytes.Repeat([]byte("x"), 300))
	hostile := []ListEntry{
		{Rel: "ok.txt", Size: 1},
		{Rel: "../evil"},
		{Rel: "dir", Dir: true},
		{Rel: "dir/..", Dir: true},
		{Rel: "dir/fine.txt", Size: 2},
		{Rel: `C:\evil`},
		{Rel: "/abs"},
		{Rel: "nul.txt"},
		{Rel: "con", Dir: true},
		{Rel: "con/inside.txt"},
		{Rel: "orphan/child.txt"},
		{Rel: long},
		{Rel: "trailing. "},
		{Rel: "a\x01b"},
		{Rel: "neg", Size: -5},
	}
	_, got, err = DecodeList(EncodeList(1, hostile))
	if err != nil || len(got) != 3 || got[0].Index != 0 || got[1].Index != 2 || got[2].Index != 4 {
		t.Fatal(got, err)
	}
	if _, _, err := DecodeList(EncodeList(1, hostile)[:40]); err == nil {
		t.Fatal("truncated list accepted")
	}
}

func TestClipFetch(t *testing.T) {
	src := t.TempDir()
	big := make([]byte, 10<<20+12345) // several windows
	for i := range big {
		big[i] = byte(i * 7 >> 3)
	}
	os.WriteFile(filepath.Join(src, "big.bin"), big, 0644)
	os.WriteFile(filepath.Join(src, "empty"), nil, 0644)
	os.Mkdir(filepath.Join(src, "dir"), 0755)
	entries, _ := PlanList([]string{filepath.Join(src, "big.bin"), filepath.Join(src, "empty"), filepath.Join(src, "dir")})
	var srv FileServer
	srv.Offer(5, entries)

	var requests atomic.Int32
	open := func(id, index uint32) *FetchStream {
		var st *FetchStream
		size := int64(0)
		if int(index) < len(entries) {
			size = entries[index].Size
		}
		st = NewFetchStream(id, index, size, func(off int64, length uint32) error {
			requests.Add(1)
			go srv.Serve(FetchReq{ID: id, Index: index, Offset: off, Length: length}, func(typ byte, p []byte) error {
				_, _, o, status, data, ok := DecodeFetchData(append([]byte(nil), p...))
				if typ != MsgClipFetchData || !ok {
					t.Error("bad fetch data")
				}
				st.Deliver(o, status, data)
				return nil
			}, func() bool { return false })
			return nil
		})
		st.timeout = 5 * time.Second
		return st
	}

	st := open(5, 0)
	got, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatal(err, len(got))
	}
	if requests.Load() != 3 {
		t.Fatal("expected 3 windows, got", requests.Load())
	}

	// Seeking restarts from the new place; seeking to "here" is free.
	st = open(5, 0)
	head := make([]byte, 1000)
	io.ReadFull(st, head)
	if pos, _ := st.Seek(0, io.SeekCurrent); pos != 1000 {
		t.Fatal(pos)
	}
	if pos, _ := st.Seek(-100, io.SeekEnd); pos != int64(len(big)-100) {
		t.Fatal(pos)
	}
	tail, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(tail, big[len(big)-100:]) {
		t.Fatal(err, len(tail))
	}
	st.Seek(5<<20, io.SeekStart)
	mid := make([]byte, 4096)
	if _, err := io.ReadFull(st, mid); err != nil || !bytes.Equal(mid, big[5<<20:5<<20+4096]) {
		t.Fatal(err)
	}

	if b, err := io.ReadAll(open(5, 1)); err != nil || len(b) != 0 {
		t.Fatal("empty file", err, len(b))
	}
	if b, err := io.ReadAll(NewEmptyStream()); err != nil || len(b) != 0 {
		t.Fatal("empty stream", err)
	}
	// Folders, stale offers and withdrawn offers fail instead of hanging.
	for _, bad := range []*FetchStream{open(5, 2), open(4, 0), open(5, 99)} {
		if _, err := io.ReadAll(bad); err != ErrFetchAborted {
			t.Fatal("expected failure, got", err)
		}
	}
	if !srv.Clear() || srv.Clear() {
		t.Fatal("clear")
	}
	if _, err := io.ReadAll(open(5, 0)); err != ErrFetchAborted {
		t.Fatal("served after clear", err)
	}
	// Silence from the other side times out.
	quiet := NewFetchStream(1, 0, 10, func(int64, uint32) error { return nil })
	quiet.timeout = 50 * time.Millisecond
	if _, err := quiet.Read(make([]byte, 10)); err != ErrFetchAborted {
		t.Fatal("no timeout", err)
	}
}

func TestClipFormats(t *testing.T) {
	pic := bytes.Repeat([]byte{1, 2, 3, 250}, 300000) // 1.2 MB "bitmap"
	in := []ClipFormat{
		{ID: 13, Data: []byte("h\x00i\x00\x00\x00")},
		{Name: "HTML Format", Data: []byte("<b>hi</b>")},
		{ID: 8, Data: pic},
		{ID: 2, Data: []byte("bitmap handle: must be dropped")},
		{Name: "Shell IDList Array", Data: []byte("local paths: must be dropped")},
		{ID: 49999, Name: "numbered registered format: must be dropped", Data: []byte("x")},
	}
	blob := EncodeFormats(in)
	if len(blob) > len(pic)/10 {
		t.Fatal("not compressed", len(blob))
	}
	var r BlobReceiver
	var got []byte
	send := func(typ byte, p []byte) error {
		if b := r.Handle(typ, append([]byte(nil), p...)); b != nil {
			got = b
		}
		return nil
	}
	if err := SendBlob(3, blob, send, func() bool { return false }); err != nil || !bytes.Equal(got, blob) {
		t.Fatal(err, len(got), len(blob))
	}
	out, err := DecodeFormats(got)
	if err != nil || len(out) != 3 {
		t.Fatal(err, len(out))
	}
	if out[0].ID != 13 || out[1].Name != "HTML Format" || out[2].ID != 8 || !bytes.Equal(out[2].Data, pic) {
		t.Fatal("wrong formats")
	}

	// Cancelled and truncated blobs are discarded.
	got = nil
	SendBlob(4, blob, send, func() bool { return true })
	if got != nil {
		t.Fatal("cancelled blob delivered")
	}
	if _, err := DecodeFormats(blob[:len(blob)/2]); err == nil {
		t.Fatal("truncated blob accepted")
	}

	// Recognising our own data when Windows hands it back, padded.
	fp := Fingerprint(out)
	back := []ClipFormat{
		{ID: 13, Data: out[0].Data},
		{Name: "html format", Data: append(append([]byte(nil), out[1].Data...), 0, 0, 0)},
		{ID: 8, Data: append(append([]byte(nil), pic...), make([]byte, 12)...)},
		{ID: 1, Data: []byte("hi\x00")}, // a format Windows added by itself
	}
	if !fp.Matches(back) {
		t.Fatal("own data not recognised")
	}
	back[0].Data = []byte("y\x00o\x00\x00\x00")
	if fp.Matches(back) {
		t.Fatal("different text taken for our own")
	}
	if fp.Matches(back[1:2]) || (ClipPrint{}).Matches(back) {
		t.Fatal("partial or empty match")
	}
}

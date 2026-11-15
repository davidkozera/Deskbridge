package proto

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"net"
	"testing"
)

func pair(t *testing.T, spw, cpw string) (*Conn, *Conn, error, error) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	cfg, err := NewServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		c   *Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		nc, _ := ln.Accept()
		tc, _, err := ServerHandshake(nc, cfg, DeriveKey(spw))
		if err != nil {
			nc.Close()
			ch <- res{nil, err}
			return
		}
		ch <- res{NewConn(tc), nil}
	}()
	nc, _ := net.Dial("tcp", ln.Addr().String())
	tc, cerr := ClientHandshake(nc, DeriveKey(cpw))
	s := <-ch
	var cc *Conn
	if cerr == nil {
		cc = NewConn(tc)
	}
	return s.c, cc, s.err, cerr
}

func TestHandshake(t *testing.T) {
	s, c, se, ce := pair(t, "abcd-efgh", "abcd-efgh")
	if se != nil || ce != nil {
		t.Fatal(se, ce)
	}
	go s.Send(MsgRect, true, []byte("hello "), []byte("world"))
	typ, p, err := c.Read()
	if err != nil || typ != MsgRect || string(p) != "hello world" {
		t.Fatal(typ, string(p), err)
	}
	_, _, se, ce = pair(t, "abcd-efgh", "wrong")
	if se != ErrAuth || ce != ErrAuth {
		t.Fatal("bad password accepted", se, ce)
	}
}

// A maintenance connection is recognised as one, a wrong password on it is
// reported as a wrong password, and a server from before maintenance
// connections existed (which answers zeros) is told apart from both.
func TestMaintenanceHandshake(t *testing.T) {
	cfg, err := NewServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	try := func(serverPassword, clientPassword string, old bool) (string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		role := make(chan string, 1)
		go func() {
			nc, _ := ln.Accept()
			defer nc.Close()
			if old {
				tc := tls.Server(nc, cfg)
				tc.Handshake()
				io.ReadFull(tc, make([]byte, 32))
				tc.Write(make([]byte, 32))
				role <- ""
				return
			}
			_, r, _ := ServerHandshake(nc, cfg, DeriveKey(serverPassword))
			role <- r
		}()
		nc, _ := net.Dial("tcp", ln.Addr().String())
		defer nc.Close()
		_, cerr := ClientHandshakeAs(nc, DeriveKey(clientPassword), RoleMaintenance)
		return <-role, cerr
	}
	if role, err := try("abcd-efgh", "abcd-efgh", false); err != nil || role != RoleMaintenance {
		t.Fatal("maintenance connection not accepted:", role, err)
	}
	if _, err := try("abcd-efgh", "wrong", false); err != ErrAuth {
		t.Fatal("wrong password on a maintenance connection:", err)
	}
	if _, err := try("abcd-efgh", "abcd-efgh", true); err != ErrNoMaintenance {
		t.Fatal("old server not recognised:", err)
	}
}

func TestCodec(t *testing.T) {
	w, h := 300, 170
	fb := make([]byte, w*h*4)
	// flat UI-like left half, noisy gradient right half
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			if x < 128 {
				fb[o], fb[o+1], fb[o+2] = byte(y/20*30), 200, byte(x/40*50)
			} else {
				fb[o], fb[o+1], fb[o+2] = byte(x/2+rand.Intn(8)), byte(y+rand.Intn(8)), byte((x+y)/2)
			}
		}
	}
	rects := DirtyRects(fb, nil, w, h, 64)
	if len(rects) != 3 {
		t.Fatal("full frame rects", rects)
	}
	prev := append([]byte(nil), fb...)
	if r := DirtyRects(fb, prev, w, h, 64); len(r) != 0 {
		t.Fatal("unchanged frame dirty", r)
	}
	fb[(100*w+70)*4] ^= 1
	fb[(100*w+299)*4] ^= 1
	r := DirtyRects(fb, prev, w, h, 64)
	if len(r) != 2 || r[0] != (Rect{64, 64, 64, 64}) || r[1] != (Rect{256, 64, 44, 64}) {
		t.Fatal(r)
	}
	out := make([]byte, w*h*4)
	seen := map[byte]bool{}
	for _, rc := range []Rect{{0, 0, 128, 170}, {128, 0, 172, 170}} {
		enc, data := EncodeRect(fb, w, rc, 85)
		seen[enc] = true
		if err := DecodeRect(enc, data, rc, out, w, h); err != nil {
			t.Fatal(err)
		}
	}
	if !seen[EncJPEG] || !seen[EncFlate] {
		t.Fatal("expected both encodings", seen)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			for k := 0; k < 3; k++ {
				d := math.Abs(float64(fb[o+k]) - float64(out[o+k]))
				if x < 128 && d != 0 {
					t.Fatalf("lossless mismatch at %d,%d", x, y)
				}
				if d > 60 {
					t.Fatalf("jpeg too far off at %d,%d: %v", x, y, d)
				}
			}
		}
	}
	if DecodeRect(EncFlate, nil, Rect{290, 0, 20, 10}, out, w, h) == nil {
		t.Fatal("out of bounds rect accepted")
	}
}

func TestAudio(t *testing.T) {
	var src []byte
	for _, v := range []float32{0.5, -0.5, 0, 2, -2, 0} { // 2 frames, 3 channels
		src = binary.LittleEndian.AppendUint32(src, math.Float32bits(v))
	}
	got := ToStereo16(nil, src, 2, 3, 32, true)
	want := []int16{16383, -16383, 32767, -32767}
	for i, wv := range want {
		if g := int16(binary.LittleEndian.Uint16(got[i*2:])); g != wv {
			t.Fatal(i, g, wv)
		}
	}
	mono := []byte{1, 0, 2, 0}
	if !bytes.Equal(ToStereo16(nil, mono, 2, 1, 16, false), []byte{1, 0, 1, 0, 2, 0, 2, 0}) {
		t.Fatal("mono")
	}
}

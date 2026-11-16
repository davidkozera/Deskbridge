package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"deskbridge/internal/proto"
)

func TestCodes(t *testing.T) {
	a, k, err := ParseCode("  https://relay.example.net/#0123456789abcdef0123 \r\n")
	if err != nil || a != "https://relay.example.net" || k != "0123456789abcdef0123" {
		t.Fatal(a, k, err)
	}
	for _, bad := range []string{
		"", "https://relay.example.net", "https://relay.example.net#short", "http://relay.example.net#0123456789abcdef0123",
		"relay.example.net#0123456789abcdef0123", "https://relay.example.net/path#0123456789abcdef0123",
		"https://relay.example.net#0123456789 abcdef0123", "https://a|b#0123456789abcdef0123",
	} {
		if _, _, err := ParseCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, _, err := ParseCode("http://localhost:8080#0123456789abcdef0123"); err != nil {
		t.Error("this PC over http should be allowed:", err)
	}
	if !ValidID(NewID()) || ValidID("xyz") || ValidID(NewID()+"0") {
		t.Error("server IDs")
	}
}

// A real Deskbridge handshake and a good deal of data, both ways, through a
// relay: the server end listening, the client end connecting.
func TestThroughRelay(t *testing.T) {
	const key = "0123456789abcdef-test-key"
	srv := NewServer(key)
	web := httptest.NewServer(srv)
	defer web.Close()
	// httptest serves plain http on 127.0.0.1, which CheckAddress allows.
	address := web.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id := NewID()
	if _, err := Connect(ctx, address, key, id); !errors.Is(err, ErrAway) {
		t.Fatal("a PC that is not listening:", err)
	}
	if _, err := Connect(ctx, address, "the-wrong-key-entirely", id); !errors.Is(err, ErrKey) {
		t.Fatal("a wrong key:", err)
	}

	password := proto.DeriveKey("correct horse")
	cfg, err := proto.NewServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	up := make(chan bool, 4)
	var seen sync.Map
	go Listen(ctx, address, key, id, func(nc net.Conn) {
		defer nc.Close()
		seen.Store(nc.RemoteAddr().String(), true)
		tc, role, err := proto.ServerHandshake(nc, cfg, password)
		if err != nil || role != proto.RoleClient {
			return
		}
		io.Copy(tc, tc) // echo
	}, func(connected bool, note string) {
		if connected {
			up <- true
		}
	})
	select {
	case <-up:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never reached the relay")
	}

	// Two clients at once, each with its own session.
	var wg sync.WaitGroup
	for n := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nc, err := Connect(ctx, address, key, id)
			if err != nil {
				t.Error(err)
				return
			}
			defer nc.Close()
			tc, err := proto.ClientHandshake(nc, password)
			if err != nil {
				t.Error("handshake through the relay:", err)
				return
			}
			data := make([]byte, 3<<20+n*777)
			rand.Read(data)
			go tc.Write(data)
			back := make([]byte, len(data))
			tc.SetReadDeadline(time.Now().Add(20 * time.Second))
			if _, err := io.ReadFull(tc, back); err != nil || !bytes.Equal(back, data) {
				t.Error("data did not come back intact:", err)
			}
		}()
	}
	wg.Wait()
	if _, ok := seen.Load("127.0.0.1:0"); !ok {
		t.Error("the server was not told where its caller came from")
	}

	// A wrong password is still refused by the server, through the relay.
	nc, err := Connect(ctx, address, key, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ClientHandshake(nc, proto.DeriveKey("wrong")); !errors.Is(err, proto.ErrAuth) {
		t.Error("a wrong password through the relay:", err)
	}
	nc.Close()

	// When the server leaves, callers are told it is away.
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := Connect(context.Background(), address, key, id)
		if errors.Is(err, ErrAway) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay still thinks the server is there:", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

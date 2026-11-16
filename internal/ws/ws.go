// Package ws is a small WebSocket implementation (RFC 6455), enough to carry
// a Deskbridge connection through anything that passes ordinary web
// traffic: a hosted container's HTTPS front door, a hotel's network.
//
// A Conn is used as a plain stream of bytes, like a TCP connection: what is
// written at one end is read at the other, and where one WebSocket message
// ends and the next begins means nothing.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	opContinue = 0
	opText     = 1
	opBinary   = 2
	opClose    = 8
	opPing     = 9
	opPong     = 10

	maxFrame = 64 << 10 // the most sent in one frame
	// maxIncoming bounds a frame's announced length, so a hostile peer
	// cannot claim an absurd one. Frames are read as a stream, so this
	// costs no memory.
	maxIncoming = 1 << 30
)

// Conn is one end of a WebSocket connection. It is a net.Conn.
type Conn struct {
	nc     net.Conn
	br     *bufio.Reader
	client bool // clients mask what they send

	// Idle, when set, makes Read fail once nothing at all (not even a
	// ping) has arrived for that long. Do not use it together with read
	// deadlines of your own.
	Idle time.Duration
	// Remote, when set, is what RemoteAddr reports.
	Remote net.Addr

	// Reading (one reader at a time).
	remain  int64
	masked  bool
	mask    [4]byte
	maskPos int

	wmu    sync.Mutex
	wbuf   []byte
	closed atomic.Bool
}

// StatusError is the answer of a server that did not accept the connection.
type StatusError struct {
	Code int
	Text string
}

func (e *StatusError) Error() string {
	if e.Text != "" {
		return fmt.Sprintf("the server answered %d: %s", e.Code, e.Text)
	}
	return fmt.Sprintf("the server answered %d", e.Code)
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Dial opens a WebSocket connection to an http, https, ws or wss address.
// header holds extra request headers. The context bounds the whole attempt.
func Dial(ctx context.Context, address string, header http.Header) (*Conn, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	secure := false
	switch u.Scheme {
	case "https", "wss":
		secure = true
	case "http", "ws":
	default:
		return nil, errors.New("the address must begin with https://")
	}
	host := u.Host
	if u.Port() == "" {
		if secure {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	if deadline, ok := ctx.Deadline(); ok {
		nc.SetDeadline(deadline)
	}
	if secure {
		// HTTP/1.1 only: a WebSocket cannot be started over HTTP/2 this way.
		tc := tls.Client(nc, &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	key := base64.StdEncoding.EncodeToString(raw)
	path := u.RequestURI()
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, u.Host, key)
	for name, values := range header {
		for _, v := range values {
			if strings.ContainsAny(name+v, "\r\n") {
				nc.Close()
				return nil, errors.New("a header holds a line break")
			}
			fmt.Fprintf(&req, "%s: %s\r\n", name, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(nc, req.String()); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(nc, 32<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		resp.Body.Close()
		nc.Close()
		return nil, &StatusError{resp.StatusCode, strings.TrimSpace(string(text))}
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		nc.Close()
		return nil, errors.New("the server did not answer as a WebSocket server")
	}
	nc.SetDeadline(time.Time{})
	return &Conn{nc: nc, br: br, client: true}, nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// IsUpgrade reports whether a request asks to become a WebSocket.
func IsUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && headerHas(r.Header, "Connection", "upgrade") && headerHas(r.Header, "Upgrade", "websocket") &&
		r.Header.Get("Sec-WebSocket-Key") != ""
}

// Accept turns an HTTP request into a WebSocket connection. After it
// succeeds the connection belongs to the caller, and nothing more may be
// written to w.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !IsUpgrade(r) {
		http.Error(w, "this address is for WebSocket connections", http.StatusBadRequest)
		return nil, errors.New("not a WebSocket request")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket connections are not possible here", http.StatusInternalServerError)
		return nil, errors.New("the connection cannot be taken over")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	nc.SetDeadline(time.Time{})
	answer := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	if _, err := io.WriteString(nc, answer); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{nc: nc, br: rw.Reader}, nil
}

// frame sends one frame. Callers hold wmu.
func (c *Conn) frame(op byte, payload []byte) error {
	b := append(c.wbuf[:0], 0x80|op)
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b = append(b, maskBit|byte(n))
	case n < 1<<16:
		b = append(b, maskBit|126, byte(n>>8), byte(n))
	default:
		b = append(b, maskBit|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	if c.client {
		var mask [4]byte
		rand.Read(mask[:])
		b = append(b, mask[:]...)
		start := len(b)
		b = append(b, payload...)
		for i := start; i < len(b); i++ {
			b[i] ^= mask[(i-start)&3]
		}
	} else {
		b = append(b, payload...)
	}
	c.wbuf = b[:0]
	_, err := c.nc.Write(b)
	return err
}

// Write sends bytes to the other end.
func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	sent := 0
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		if err := c.frame(opBinary, p[:n]); err != nil {
			return sent, err
		}
		sent += n
		p = p[n:]
	}
	return sent, nil
}

// Ping asks the other end to show it is still there. Its answer counts as
// something arriving, for Idle at this end; the ping does at the other.
func (c *Conn) Ping() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.frame(opPing, nil)
}

// header reads frame headers until one announces data, dealing with pings
// and the like on the way.
func (c *Conn) header() error {
	for {
		if c.Idle > 0 {
			c.nc.SetReadDeadline(time.Now().Add(c.Idle))
		}
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return err
		}
		op := h[0] & 0x0F
		c.masked = h[1]&0x80 != 0
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return err
			}
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return err
			}
			v := binary.BigEndian.Uint64(b[:])
			if v > maxIncoming {
				return errors.New("websocket: a frame is too large")
			}
			n = int64(v)
		}
		if h[0]&0x70 != 0 {
			return errors.New("websocket: unexpected extension bits")
		}
		if c.masked {
			if _, err := io.ReadFull(c.br, c.mask[:]); err != nil {
				return err
			}
		}
		c.maskPos = 0
		switch op {
		case opContinue, opText, opBinary:
			if n == 0 {
				continue
			}
			c.remain = n
			return nil
		case opPing, opPong, opClose:
			if n > 125 {
				return errors.New("websocket: a control frame is too large")
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(c.br, body); err != nil {
				return err
			}
			if c.masked {
				for i := range body {
					body[i] ^= c.mask[i&3]
				}
			}
			switch op {
			case opPing:
				c.wmu.Lock()
				err := c.frame(opPong, body)
				c.wmu.Unlock()
				if err != nil {
					return err
				}
			case opClose:
				return io.EOF
			}
		default:
			return errors.New("websocket: unknown kind of frame")
		}
	}
}

// Read receives bytes from the other end.
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.remain == 0 {
		if err := c.header(); err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > c.remain {
		p = p[:c.remain]
	}
	n, err := c.br.Read(p)
	if c.masked {
		for i := range n {
			p[i] ^= c.mask[(c.maskPos+i)&3]
		}
		c.maskPos = (c.maskPos + n) & 3
	}
	c.remain -= int64(n)
	if n > 0 {
		err = nil // report it with the next read
	}
	return n, err
}

// Close says goodbye, if the other end is still listening, and closes.
func (c *Conn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	go func() {
		// Best effort, and never in the way of the caller: a writer may
		// be stuck holding the lock, which closing below sets free.
		if c.wmu.TryLock() {
			c.nc.SetWriteDeadline(time.Now().Add(time.Second))
			c.frame(opClose, []byte{0x03, 0xE8}) // 1000: normal
			c.wmu.Unlock()
		}
		c.nc.Close()
	}()
	// Unblock anything reading or writing right away.
	c.nc.SetDeadline(time.Now())
	return nil
}

func (c *Conn) LocalAddr() net.Addr { return c.nc.LocalAddr() }

func (c *Conn) RemoteAddr() net.Addr {
	if c.Remote != nil {
		return c.Remote
	}
	return c.nc.RemoteAddr()
}

func (c *Conn) SetDeadline(t time.Time) error      { return c.nc.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.nc.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.nc.SetWriteDeadline(t) }

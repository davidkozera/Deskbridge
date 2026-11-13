// Package proto holds everything that is not Windows-specific: message
// framing, the password handshake, the screen codec and audio conversion.
package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const DefaultPort = "47600"

// Server -> client
const (
	MsgSize     = 1 // w u32, h u32
	MsgRect     = 2 // x,y,w,h u16, enc u8, data
	MsgFrameEnd = 3
	MsgCursor   = 4 // system cursor id u16
	MsgAudioFmt = 5 // sample rate u32 (always 16-bit stereo)
	MsgAudio    = 6 // s16le stereo PCM
)

// Client -> server
const (
	MsgAck       = 16
	MsgMouseMove = 17 // x,y u16
	MsgMouseBtn  = 18 // button u8 (0=L 1=R 2=M 3=X1 4=X2), down u8, x,y u16
	MsgWheel     = 19 // delta i16, horizontal u8
	MsgKey       = 20 // vk u16, scan u16, flags u8 (1=extended, 2=up)
	MsgSettings  = 21 // fps u8, jpeg quality u8, audio on u8, allow sleep u8
)

const maxMsg = 64 << 20

type Conn struct {
	nc net.Conn
	r  *bufio.Reader
	mu sync.Mutex
	w  *bufio.Writer
}

func NewConn(nc net.Conn) *Conn {
	return &Conn{nc: nc, r: bufio.NewReaderSize(nc, 256<<10), w: bufio.NewWriterSize(nc, 256<<10)}
}

// Send writes one message made of the given parts. Safe for concurrent use.
func (c *Conn) Send(t byte, flush bool, parts ...[]byte) error {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	var hdr [5]byte
	hdr[0] = t
	binary.LittleEndian.PutUint32(hdr[1:], uint32(n))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	for _, p := range parts {
		if _, err := c.w.Write(p); err != nil {
			return err
		}
	}
	if flush {
		return c.w.Flush()
	}
	return nil
}

func (c *Conn) Read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[1:])
	if n > maxMsg {
		return 0, nil, errors.New("message too large")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

func (c *Conn) Close() error { return c.nc.Close() }

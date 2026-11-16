package relay

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"deskbridge/internal/ws"
)

// A relay code is what the person copies from wherever they set the relay
// up and pastes into Deskbridge: the relay's address and its key in one
// piece, "https://relay.example.net#the-key".

// ParseCode splits a relay code into the relay's address and key.
func ParseCode(code string) (address, key string, err error) {
	code = strings.TrimSpace(code)
	i := strings.LastIndexByte(code, '#')
	if i < 0 {
		return "", "", errors.New("a relay code looks like https://address#key")
	}
	address, key = strings.TrimRight(strings.TrimSpace(code[:i]), "/"), strings.TrimSpace(code[i+1:])
	if err := CheckAddress(address); err != nil {
		return "", "", err
	}
	if len(key) < MinKey || len(key) > 200 || strings.ContainsAny(key, " \t\r\n") {
		return "", "", errors.New("the key in the relay code is not the right shape")
	}
	return address, key, nil
}

// CheckAddress reports whether address is a usable relay address. It must
// be https, so the relay key is never sent in the clear; plain http is
// allowed only to this same PC, for trying things out.
func CheckAddress(address string) error {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || len(address) > 300 || strings.ContainsAny(address, " \t\r\n|") {
		return errors.New("the relay's address is not a web address")
	}
	if u.Path != "" || u.RawQuery != "" || u.User != nil {
		return errors.New("the relay's address should be just https:// and a name")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"):
	default:
		return errors.New("the relay's address must begin with https://")
	}
	return nil
}

var (
	// ErrKey means the relay did not accept the relay key.
	ErrKey = errors.New("the relay did not accept the relay key")
	// ErrAway means the wanted PC is not connected to the relay: it is
	// off, asleep, offline, or not set up to use this relay.
	ErrAway = errors.New("that PC is not connected to the relay")
)

func dial(ctx context.Context, address, key, path string) (*ws.Conn, error) {
	c, err := ws.Dial(ctx, address+path, http.Header{keyHeader: {key}})
	var status *ws.StatusError
	if errors.As(err, &status) {
		switch status.Code {
		case http.StatusForbidden:
			return nil, ErrKey
		case http.StatusNotFound:
			return nil, ErrAway
		}
	}
	return c, err
}

// Connect reaches the server with the given ID through the relay. What
// comes back behaves like a direct connection to that server.
func Connect(ctx context.Context, address, key, id string) (net.Conn, error) {
	c, err := dial(ctx, address, key, "/v1/connect?id="+url.QueryEscape(id))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// addr is the far end of a relayed connection, as the relay reported it.
type addr string

func (a addr) Network() string { return "relay" }
func (a addr) String() string  { return net.JoinHostPort(string(a), "0") }

// Listen keeps a server known to the relay under id until ctx ends, and
// calls serve (on a goroutine of its own) with each connection a client
// makes through it. state is told, in words for the log, how it is going.
// It returns only when ctx ends.
func Listen(ctx context.Context, address, key, id string, serve func(net.Conn), state func(connected bool, note string)) {
	wait := 2 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := listenOnce(ctx, address, key, id, serve, state)
		if ctx.Err() != nil {
			return
		}
		state(false, err.Error())
		if time.Since(began) > time.Minute {
			wait = 2 * time.Second // it was working; try again soon
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

func listenOnce(ctx context.Context, address, key, id string, serve func(net.Conn), state func(bool, string)) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	c, err := dial(dctx, address, key, "/v1/listen?id="+url.QueryEscape(id))
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	state(true, "")
	c.Idle = listenIdle
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(25 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				c.Close()
				return
			case <-stop:
				return
			case <-tick.C:
				// Its answer keeps this end from going Idle, and the
				// ping itself does the same for the relay's end.
				if c.Ping() != nil {
					return
				}
			}
		}
	}()
	lines := bufio.NewScanner(c)
	lines.Buffer(make([]byte, 1024), 1024)
	for lines.Scan() {
		parts := strings.Fields(lines.Text())
		if len(parts) != 3 || parts[0] != "incoming" || !ValidID(parts[1]) {
			continue
		}
		from := "internet"
		if ip := net.ParseIP(parts[2]); ip != nil {
			from = ip.String()
		}
		go func() {
			actx, cancel := context.WithTimeout(ctx, acceptWait)
			defer cancel()
			s, err := dial(actx, address, key, "/v1/accept?token="+parts[1])
			if err != nil {
				return
			}
			s.Remote = addr(from)
			serve(s)
		}()
	}
	if err := lines.Err(); err != nil {
		return err
	}
	return errors.New("the relay closed the connection")
}

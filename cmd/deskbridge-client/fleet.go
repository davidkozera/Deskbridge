//go:build windows

package main

// Looking after every saved server, not only the one being used.
//
// When the client starts, and a few times a day after that, it asks each
// saved connection that has a remembered password which version it runs.
// It does so over a "maintenance" connection, which shows and controls
// nothing and leaves alone whoever may be using that PC remotely. Servers
// that are behind are counted on the Update button, and one click updates
// them all at the same time, along with the connected server and this PC.
//
// Servers older than 1.5.0 do not know maintenance connections. They are
// taken to need the update, and when Update is clicked they are sent it
// over an ordinary connection instead.

import (
	"crypto/tls"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deskbridge/internal/proto"
	"deskbridge/internal/update"
)

const wmCheckServers = wmApp + 17 // main: time to ask the saved servers for their versions

// What is known about a saved server.
const (
	serverAnswered    = iota // it said which version it runs
	serverOld                // it does not know maintenance connections: older than 1.5.0
	serverUnreachable        // off, asleep or not on this network
	serverBadPassword        // the saved password was not accepted
)

type savedServer struct {
	name, host, port, password string
	state                      int
	version                    string
}

func (o savedServer) key() string { return strings.ToLower(net.JoinHostPort(o.host, o.port)) }

var (
	savedServers atomic.Pointer[[]savedServer] // what the last check found
	checking     atomic.Bool
)

// checkServers asks every saved server for its version, in the background.
// Runs on the UI thread.
func checkServers() {
	if _, ok := update.Parse(proto.Version); !ok {
		return // a development build
	}
	if updating {
		return // the update under way keeps its own record of what it did
	}
	if !checking.CompareAndSwap(false, true) {
		return
	}
	var list []savedServer
	seen := map[string]bool{}
	for _, c := range computers {
		o := savedServer{name: c.name, host: c.host, port: c.port}
		if pw := dpapi(pCryptUnprotectData, c.password); pw != nil && !seen[o.key()] {
			seen[o.key()] = true
			o.password = string(pw)
			list = append(list, o)
		}
	}
	// Open tabs count too, saved or not: their passwords are at hand.
	for _, tb := range tabs {
		o := savedServer{name: tb.target.name, host: tb.target.host, port: tb.target.port, password: tb.target.password}
		if !seen[o.key()] {
			seen[o.key()] = true
			list = append(list, o)
		}
	}
	before := savedServers.Load()
	go func() {
		defer checking.Store(false)
		var wg sync.WaitGroup
		for i := range list {
			wg.Add(1)
			go func(o *savedServer) {
				defer wg.Done()
				m, state := openMaintenance(*o, false)
				o.state = state
				if m != nil {
					o.version = m.version
					m.close()
				}
				// An old server and a wrong password look the same from
				// here. Once an update attempt has shown it to be the
				// password, do not keep offering to update that server.
				if state == serverOld && before != nil {
					for _, b := range *before {
						if b.key() == o.key() && b.password == o.password && b.state == serverBadPassword {
							o.state = serverBadPassword
						}
					}
				}
			}(&list[i])
		}
		wg.Wait()
		savedServers.Store(&list)
		pPostMessage.Call(hMain, wmUpdateInfo, 0, 0)
	}()
}

// behind picks out the saved servers that should be updated to target,
// leaving out the connected ones, which are handled over their own
// connections. Runs on the UI thread.
func behind(target string) []savedServer {
	list := savedServers.Load()
	if list == nil {
		return nil
	}
	connected := map[string]bool{}
	for _, tb := range tabs {
		if tb.l != nil {
			connected[savedServer{host: tb.target.host, port: tb.target.port}.key()] = true
		}
	}
	var out []savedServer
	for _, o := range *list {
		if connected[o.key()] {
			continue
		}
		switch o.state {
		case serverAnswered:
			if o.version != "" && update.Allowed(o.version, target) {
				out = append(out, o)
			}
		case serverOld:
			if update.Allowed("1.4.0", target) {
				out = append(out, o)
			}
		}
	}
	return out
}

// maintenance is an open connection to a server for asking its version and
// handing it an update.
type maintenance struct {
	tc      *tls.Conn
	c       *proto.Conn
	version string // empty if the server did not say
	result  chan []byte
	done    chan struct{}
}

func (m *maintenance) close() { m.tc.Close() }

// openMaintenance connects to a saved server. With takeOver it falls back,
// for a server too old for maintenance connections, to an ordinary
// connection, which takes the place of anyone connected to it.
func openMaintenance(o savedServer, takeOver bool) (*maintenance, int) {
	key := proto.DeriveKey(o.password)
	dial := func(role string) (*tls.Conn, error) {
		nc, err := net.DialTimeout("tcp", net.JoinHostPort(o.host, o.port), 4*time.Second)
		if err != nil {
			return nil, err
		}
		tc, err := proto.ClientHandshakeAs(nc, key, role)
		if err != nil {
			nc.Close()
			return nil, err
		}
		return tc, nil
	}
	tc, err := dial(proto.RoleMaintenance)
	if errors.Is(err, proto.ErrNoMaintenance) {
		if !takeOver {
			return nil, serverOld
		}
		tc, err = dial(proto.RoleClient)
	}
	switch {
	case errors.Is(err, proto.ErrAuth):
		return nil, serverBadPassword
	case err != nil:
		return nil, serverUnreachable
	}
	m := &maintenance{tc: tc, c: proto.NewConn(tc), result: make(chan []byte, 1), done: make(chan struct{})}
	version := make(chan string, 1)
	go func() {
		// Everything else an ordinary connection sends (the picture, the
		// pointer) is read and dropped.
		defer close(m.done)
		for {
			t, p, err := m.c.Read()
			if err != nil {
				return
			}
			switch {
			case t == proto.MsgNotice && len(p) >= 2 && len(p) < 64 && p[0] == proto.NoticeVersion:
				select {
				case version <- string(p[1:]):
				default:
				}
			case t == proto.MsgUpdateResult:
				select {
				case m.result <- append([]byte(nil), p...):
				default:
				}
			}
		}
	}()
	select {
	case m.version = <-version:
	case <-m.done:
		m.close()
		return nil, serverUnreachable
	case <-time.After(8 * time.Second):
		// Servers older than 1.1.0 do not say.
	}
	return m, serverAnswered
}

// push sends the setup program and waits for the server's verdict.
func (m *maintenance) push(version, signature string, installer []byte) (ok bool, why string) {
	send := func(t byte, p []byte) bool { return m.c.Send(t, true, p) == nil }
	sent := send(proto.MsgUpdateBegin, []byte(version+"\n"+signature+"\n"+strconv.Itoa(len(installer))))
	for off := 0; sent && off < len(installer); off += 256 << 10 {
		sent = send(proto.MsgUpdateData, installer[off:min(off+256<<10, len(installer))])
	}
	sent = sent && send(proto.MsgUpdateEnd, nil)
	// An answer may be waiting even if sending was cut short (a refusal).
	wait := 2 * time.Minute
	if !sent {
		wait = 2 * time.Second
	}
	select {
	case r := <-m.result:
		if len(r) > 0 && r[0] == 1 {
			return true, ""
		}
		if len(r) > 1 {
			return false, string(r[1:])
		}
		return false, "it gave no reason."
	case <-m.done:
		return false, "the connection closed."
	case <-time.After(wait):
		if !sent {
			return false, "the connection closed."
		}
		return false, "it did not answer."
	}
}

// updateServer brings one saved server up to version. It returns a line for
// the pane and what is now known about the server.
func updateServer(o savedServer, version, signature string, installer []byte) (string, savedServer) {
	m, state := openMaintenance(o, true)
	o.state = state
	switch state {
	case serverUnreachable:
		return o.name + ": could not be reached.", o
	case serverBadPassword:
		return o.name + ": its saved password was not accepted.", o
	}
	defer m.close()
	o.version = m.version
	if m.version != "" && !update.Allowed(m.version, version) {
		if m.version == version {
			return o.name + ": already runs version " + m.version + ".", o
		}
		return o.name + ": runs version " + m.version + ", which cannot be replaced by version " + version + ".", o
	}
	ok, why := m.push(version, signature, installer)
	if !ok {
		return o.name + ": not updated: " + why, o
	}
	o.version = version
	return o.name + ": installing version " + version + ".", o
}

// updateServers updates several saved servers at the same time and returns
// what happened, a line for each.
func updateServers(list []savedServer, version, signature string, installer []byte) string {
	lines := make([]string, len(list))
	after := make([]savedServer, len(list))
	var wg sync.WaitGroup
	for i, o := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lines[i], after[i] = updateServer(o, version, signature, installer)
		}()
	}
	wg.Wait()
	// Remember the outcome, so the button stops counting the ones done.
	if known := savedServers.Load(); known != nil {
		merged := append([]savedServer(nil), *known...)
		for _, a := range after {
			for j := range merged {
				if merged[j].key() == a.key() {
					merged[j].state, merged[j].version = a.state, a.version
				}
			}
		}
		savedServers.Store(&merged)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\r\n")
}

// Package relay lets two PCs that can only make outgoing connections reach
// each other across the internet.
//
// A relay runs somewhere both can reach (see cmd/deskbridge-relay). Each
// Deskbridge server keeps one quiet connection open to it, saying "I am the
// PC with this ID". A client asks the relay for that ID; the relay tells
// the server, the server makes a second outgoing connection for the
// session, and the relay passes bytes between the two.
//
// The relay never sees inside a session: what it carries is Deskbridge's
// own encrypted connection, password check included, exactly as on a local
// network. The relay key only decides who may use the relay at all.
//
// Everything travels as WebSocket over HTTPS, so it passes wherever web
// traffic does, and can be hosted behind an ordinary web front door.
package relay

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"deskbridge/internal/ws"
)

const (
	keyHeader = "X-Deskbridge-Relay-Key"

	// MinKey is the shortest relay key accepted.
	MinKey = 16

	listenIdle  = 90 * time.Second // a waiting server pings every 25 seconds
	sessionIdle = 2 * time.Minute  // both ends of a session exchange pings every few seconds
	acceptWait  = 15 * time.Second // how long a server has to pick up a caller

	maxListeners = 200
	maxSessions  = 200
)

// ValidID reports whether id is a well-formed server ID: 32 hex digits.
func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewID makes a server ID.
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Server is a relay. Use it as an http.Handler.
type Server struct {
	key []byte
	// Log, if set, is told about comings and goings.
	Log func(format string, args ...any)

	mu        sync.Mutex
	listeners map[string]*ws.Conn      // server ID -> its waiting connection
	pending   map[string]chan *ws.Conn // token -> where the server's session connection is handed over
	sessions  int
}

// NewServer makes a relay that admits whoever knows key.
func NewServer(key string) *Server {
	return &Server{key: []byte(key), listeners: map[string]*ws.Conn{}, pending: map[string]chan *ws.Conn{}}
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

func (s *Server) admit(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(keyHeader)), s.key) == 1 {
		return true
	}
	time.Sleep(500 * time.Millisecond) // slow down guessing
	http.Error(w, "the relay key is wrong", http.StatusForbidden)
	return false
}

// caller is the address a request came from, as far as it can be known
// behind a web front door.
func caller(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first, _, _ := strings.Cut(f, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return "unknown"
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		io.WriteString(w, "Deskbridge relay. Nothing to see here: Deskbridge servers and clients connect to this address.\n")
	case "/v1/listen":
		s.listen(w, r)
	case "/v1/connect":
		s.connect(w, r)
	case "/v1/accept":
		s.accept(w, r)
	default:
		http.NotFound(w, r)
	}
}

// listen is a server announcing itself and waiting for callers.
func (s *Server) listen(w http.ResponseWriter, r *http.Request) {
	if !s.admit(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if !ValidID(id) {
		http.Error(w, "that is not a server ID", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, known := s.listeners[id]
	full := !known && len(s.listeners) >= maxListeners
	s.mu.Unlock()
	if full {
		http.Error(w, "the relay is serving as many PCs as it can", http.StatusServiceUnavailable)
		return
	}
	c, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	c.Idle = listenIdle
	s.mu.Lock()
	old := s.listeners[id]
	s.listeners[id] = c
	s.mu.Unlock()
	if old != nil {
		old.Close() // the same PC connecting afresh
	}
	s.logf("%s: waiting for callers (from %s)", id[:8], caller(r))
	io.Copy(io.Discard, c) // nothing is expected; this ends when it goes away
	c.Close()
	s.mu.Lock()
	if s.listeners[id] == c {
		delete(s.listeners, id)
	}
	s.mu.Unlock()
	s.logf("%s: gone", id[:8])
}

// connect is a client asking for a server.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if !s.admit(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if !ValidID(id) {
		http.Error(w, "that is not a server ID", http.StatusBadRequest)
		return
	}
	token := NewID()
	arrival := make(chan *ws.Conn, 1)
	s.mu.Lock()
	target := s.listeners[id]
	full := s.sessions >= maxSessions
	if target != nil && !full {
		s.pending[token] = arrival
		s.sessions++
	}
	s.mu.Unlock()
	if target == nil {
		http.Error(w, "that PC is not connected to the relay", http.StatusNotFound)
		return
	}
	if full {
		http.Error(w, "the relay is carrying as many sessions as it can", http.StatusServiceUnavailable)
		return
	}
	defer func() {
		s.mu.Lock()
		s.sessions--
		s.mu.Unlock()
	}()
	giveUp := func() {
		s.mu.Lock()
		delete(s.pending, token)
		s.mu.Unlock()
	}
	client, err := ws.Accept(w, r)
	if err != nil {
		giveUp()
		return
	}
	from := caller(r)
	target.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = fmt.Fprintf(target, "incoming %s %s\n", token, from)
	target.SetWriteDeadline(time.Time{})
	if err != nil {
		giveUp()
		client.Close()
		return
	}
	var server *ws.Conn
	select {
	case server = <-arrival:
	case <-time.After(acceptWait):
		s.mu.Lock()
		_, waiting := s.pending[token]
		delete(s.pending, token)
		s.mu.Unlock()
		if waiting {
			client.Close()
			s.logf("%s: did not pick up a caller from %s", id[:8], from)
			return
		}
		server = <-arrival // it arrived at the last moment
	}
	if server == nil {
		client.Close()
		return
	}
	s.logf("%s: session with %s begins", id[:8], from)
	client.Idle, server.Idle = sessionIdle, sessionIdle
	done := make(chan struct{}, 2)
	pass := func(to, from *ws.Conn) {
		io.CopyBuffer(to, from, make([]byte, 64<<10))
		done <- struct{}{}
	}
	go pass(server, client)
	go pass(client, server)
	<-done
	client.Close()
	server.Close()
	<-done
	s.logf("%s: session with %s ends", id[:8], from)
}

// accept is a server's second connection, picking up a caller it was told
// about.
func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	if !s.admit(w, r) {
		return
	}
	token := r.URL.Query().Get("token")
	s.mu.Lock()
	arrival := s.pending[token]
	delete(s.pending, token)
	s.mu.Unlock()
	if arrival == nil {
		http.Error(w, "that caller is no longer waiting", http.StatusNotFound)
		return
	}
	c, err := ws.Accept(w, r)
	if err != nil {
		// Nothing arrives, and the caller is dropped when its wait runs
		// out; hand over a dead connection so that is immediate.
		close(arrival)
		return
	}
	arrival <- c
}

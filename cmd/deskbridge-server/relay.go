//go:build windows

package main

// Being reachable across the internet, through a relay.
//
// Normally a client connects straight to this PC, which only works on the
// same network. With a relay set (see internal/relay), this PC also keeps
// one outgoing connection open to it, and clients elsewhere reach this PC
// through that. A session arriving that way is served exactly like any
// other: the same encryption, the same password check, the same rules about
// who is in control. The relay passes it along without seeing inside.
//
// The relay's address and key arrive from a client that is connected, and
// has therefore already given this PC's password. They are kept next to the
// password, with an ID made here that tells the relay which PC this is.

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"deskbridge/internal/relay"
)

const relayFile = "relay.txt"

type relaySettings struct{ address, key, id string }

var (
	relayMu    sync.Mutex
	relayNow   relaySettings
	relayStop  context.CancelFunc
	relayServe func(net.Conn)
)

func relayPath() string {
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), relayFile)
}

func loadRelay() relaySettings {
	data, err := os.ReadFile(relayPath())
	if err != nil {
		return relaySettings{}
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	if len(lines) < 3 {
		return relaySettings{}
	}
	r := relaySettings{strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])}
	if !relay.ValidID(r.id) {
		return relaySettings{}
	}
	if r.address != "" && (relay.CheckAddress(r.address) != nil || len(r.key) < relay.MinKey) {
		r.address, r.key = "", "" // keep the ID, so saved connections still match if it is set again
	}
	return r
}

func saveRelay(r relaySettings) error {
	return os.WriteFile(relayPath(), []byte(r.address+"\r\n"+r.key+"\r\n"+r.id+"\r\n"), 0600)
}

// startRelay begins using the saved relay, if there is one. serve is what
// handles a connection, however it arrived.
func startRelay(serve func(net.Conn)) {
	relayMu.Lock()
	defer relayMu.Unlock()
	relayServe = serve
	relayNow = loadRelay()
	runRelay()
}

// runRelay (re)starts the connection to the relay to match relayNow.
// Callers hold relayMu.
func runRelay() {
	if relayStop != nil {
		relayStop()
		relayStop = nil
	}
	r := relayNow
	if r.address == "" || relayServe == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	relayStop = cancel
	serve := relayServe
	go func() {
		last := ""
		relay.Listen(ctx, r.address, r.key, r.id, func(nc net.Conn) { serve(nc) }, func(connected bool, note string) {
			now := "relay: connected to " + r.address + "; this PC can be reached through it"
			if !connected {
				now = "relay: not connected to " + r.address + " (" + note + "); trying again"
			}
			if now != last { // not a line for every retry
				log.Print(now)
				last = now
			}
		})
	}()
}

// setRelay changes the relay this PC uses, at a connected client's request.
// An empty address stops using one.
func setRelay(address, key string) error {
	if address != "" {
		if err := relay.CheckAddress(address); err != nil {
			return err
		}
		if len(key) < relay.MinKey || len(key) > 200 || strings.ContainsAny(key, " \t\r\n") {
			return errors.New("the relay key is not the right shape")
		}
	} else {
		key = ""
	}
	relayMu.Lock()
	defer relayMu.Unlock()
	if relayNow.address == address && relayNow.key == key {
		return nil
	}
	r := relaySettings{address, key, relayNow.id}
	if !relay.ValidID(r.id) {
		r.id = relay.NewID()
	}
	if err := saveRelay(r); err != nil {
		return errors.New("the relay settings could not be saved: " + err.Error())
	}
	relayNow = r
	if address == "" {
		log.Print("relay: turned off")
	}
	runRelay()
	return nil
}

// relayInfo is what a client is told: this PC's ID at the relay and the
// relay's address ("id\naddress"), or nothing of either if no relay is set.
func relayInfo() []byte {
	relayMu.Lock()
	defer relayMu.Unlock()
	if relayNow.address == "" {
		return []byte("\n")
	}
	return []byte(relayNow.id + "\n" + relayNow.address)
}

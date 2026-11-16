// Command deskbridge-relay lets Deskbridge servers and clients that can only
// make outgoing connections meet across the internet (see internal/relay).
//
// It is a small web server. Run it somewhere both ends can reach over
// HTTPS, such as a hosted container behind the host's HTTPS front door:
//
//	DESKBRIDGE_RELAY_KEY=<a long random key>  who may use the relay
//	PORT=8080                                 where to listen (plain HTTP;
//	                                          the host adds HTTPS)
//
// It keeps nothing on disk and never sees inside a session.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"deskbridge/internal/proto"
	"deskbridge/internal/relay"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	key := os.Getenv("DESKBRIDGE_RELAY_KEY")
	if len(key) < relay.MinKey {
		log.Fatalf("Set DESKBRIDGE_RELAY_KEY to a long random key (%d characters or more).", relay.MinKey)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	s := relay.NewServer(key)
	s.Log = log.Printf
	web := &http.Server{
		Addr:              ":" + port,
		Handler:           s,
		ReadHeaderTimeout: 15 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Printf("Deskbridge relay %s listening on port %s", proto.Version, port)
	log.Fatal(web.ListenAndServe())
}

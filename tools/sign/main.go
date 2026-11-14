// Command sign signs a Deskbridge setup program so that installed copies
// will accept it as an update.
//
//	go run ./tools/sign -genkey
//	    prints a new key pair. The public key goes in
//	    internal/update/key.go; the private key is kept secret (as the
//	    GitHub repository secret UPDATE_SIGNING_KEY) and never committed.
//
//	DESKBRIDGE_SIGNING_KEY=<private key> go run ./tools/sign -version 1.2.3 Deskbridge-Setup-1.2.3.exe
//	    writes Deskbridge-Setup-1.2.3.exe.sig next to the file.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"

	"deskbridge/internal/update"
)

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "sign:", msg)
	os.Exit(1)
}

func main() {
	genkey := flag.Bool("genkey", false, "print a new key pair")
	version := flag.String("version", "", "version the setup program installs")
	flag.Parse()

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println("Private key (keep secret):", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Println("Public key:               ", base64.StdEncoding.EncodeToString(pub))
		return
	}

	if _, ok := update.Parse(*version); !ok || flag.NArg() != 1 {
		fail("usage: sign -version 1.2.3 <setup program>")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("DESKBRIDGE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		fail("DESKBRIDGE_SIGNING_KEY is not set to the private key. On GitHub, add it as the repository secret UPDATE_SIGNING_KEY.")
	}
	file := flag.Arg(0)
	installer, err := os.ReadFile(file)
	if err != nil {
		fail(err.Error())
	}
	sig := update.Sign(seed, *version, installer)
	if err := update.Verify(*version, installer, sig); err != nil {
		fail("the private key does not match the public key built into Deskbridge (internal/update/key.go)")
	}
	if err := os.WriteFile(file+".sig", []byte(sig+"\n"), 0644); err != nil {
		fail(err.Error())
	}
	fmt.Println("Signed", file)
}

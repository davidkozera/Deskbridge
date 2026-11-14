// Package update finds, downloads and checks new versions of Deskbridge.
//
// Releases are published on GitHub as a setup program plus a small signature
// file. A setup program is only ever run after its signature has been
// checked against the public key built into this package, so a release that
// was not signed with the matching private key (kept as a secret of the
// GitHub repository) is refused, wherever it came from.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository releases are published in.
const Repo = "davidkozera/Deskbridge"

// MaxInstaller bounds what is accepted as a setup program.
const MaxInstaller = 100 << 20

// apiBase and downloadPrefix are variables so tests can point them at a
// local server.
var (
	apiBase        = "https://api.github.com/repos/" + Repo
	downloadPrefix = "https://github.com/" + Repo + "/releases/download/"
)

// Parse reads a version such as "1.2.3". Anything else ("dev") is not a
// version, and such builds neither offer nor accept updates.
func Parse(v string) (n [3]int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 || x > 99999 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// Newer reports whether version a is newer than version b.
func Newer(a, b string) bool {
	x, ok1 := Parse(a)
	y, ok2 := Parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// AssetName is the setup program's file name in a release.
func AssetName(version string) string { return "Deskbridge-Setup-" + version + ".exe" }

// message is what gets signed: the version and a fingerprint of the setup
// program together, so a genuine but old setup program cannot be passed off
// as a newer version.
func message(version string, installer []byte) []byte {
	sum := sha256.Sum256(installer)
	return []byte("Deskbridge update v1\n" + version + "\n" + hex.EncodeToString(sum[:]) + "\n")
}

// Sign produces the signature file's contents. seed is the 32-byte private
// key seed.
func Sign(seed []byte, version string, installer []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), message(version, installer)))
}

var ErrBadSignature = errors.New("the update is not signed by Deskbridge's publisher and was refused")

// Verify checks that installer is the genuine setup program for version.
func Verify(version string, installer []byte, signature string) error {
	return verifyWith(PublicKey, version, installer, signature)
}

func verifyWith(publicKey, version string, installer []byte, signature string) error {
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("this build has no update key, so it cannot check updates")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	if _, ok := Parse(version); !ok || !ed25519.Verify(pub, message(version, installer), sig) {
		return ErrBadSignature
	}
	return nil
}

// Release is a published version.
type Release struct {
	Version      string
	installerURL string
	sigURL       string
}

var client = &http.Client{Timeout: 5 * time.Minute}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Deskbridge-updater")
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("the download is larger than expected")
	}
	return data, nil
}

var errNotFound = errors.New("not found")

// ErrNoRelease means nothing has been published (or the repository is not
// public).
var ErrNoRelease = errors.New("no published version was found")

func release(ctx context.Context, path string) (Release, error) {
	data, err := get(ctx, apiBase+path, 4<<20)
	if errors.Is(err, errNotFound) {
		return Release{}, ErrNoRelease
	}
	if err != nil {
		return Release{}, err
	}
	var doc struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Release{}, errors.New("GitHub's answer could not be read")
	}
	r := Release{Version: strings.TrimPrefix(doc.Tag, "v")}
	if _, ok := Parse(r.Version); !ok {
		return Release{}, ErrNoRelease
	}
	for _, a := range doc.Assets {
		if !strings.HasPrefix(a.URL, downloadPrefix) {
			continue // only ever download from this project's releases
		}
		switch a.Name {
		case AssetName(r.Version):
			r.installerURL = a.URL
		case AssetName(r.Version) + ".sig":
			r.sigURL = a.URL
		}
	}
	if r.installerURL == "" || r.sigURL == "" {
		return Release{}, errors.New("version " + r.Version + " was published without a signed setup program")
	}
	return r, nil
}

// Latest returns the newest published version.
func Latest(ctx context.Context) (Release, error) { return release(ctx, "/releases/latest") }

// ByVersion returns a particular published version.
func ByVersion(ctx context.Context, version string) (Release, error) {
	return release(ctx, "/releases/tags/v"+version)
}

// Download fetches a release's setup program and signature, and returns them
// only if the signature is good.
func Download(ctx context.Context, r Release) (installer []byte, signature string, err error) {
	sig, err := get(ctx, r.sigURL, 4096)
	if err != nil {
		return nil, "", fmt.Errorf("downloading the signature: %w", err)
	}
	installer, err = get(ctx, r.installerURL, MaxInstaller)
	if err != nil {
		return nil, "", fmt.Errorf("downloading the setup program: %w", err)
	}
	signature = string(bytes.TrimSpace(sig))
	if err := Verify(r.Version, installer, signature); err != nil {
		return nil, "", err
	}
	return installer, signature, nil
}

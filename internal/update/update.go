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

// Release channels, from the least settled to the most. A PC follows one
// channel; the more settled ones are always acceptable to it too, so a PC
// on Alpha also takes a Beta or Mainline version that is newer.
const (
	Alpha = iota
	Beta
	Mainline
)

// ChannelName is a channel's name as shown to the person.
func ChannelName(channel int) string {
	switch channel {
	case Alpha:
		return "Alpha"
	case Beta:
		return "Beta"
	}
	return "Mainline"
}

// ChannelKey is a channel's name in settings files, and ChannelFromKey reads
// it back (anything unknown is Mainline).
func ChannelKey(channel int) string { return strings.ToLower(ChannelName(channel)) }

func ChannelFromKey(key string) int {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "alpha":
		return Alpha
	case "beta":
		return Beta
	}
	return Mainline
}

// Floor is the first version that understands channels. Nothing is ever
// moved back to a version before it.
const Floor = "1.10.0"

// Version is a parsed version: "1.2.3" on Mainline, or "1.2.3-alpha.4" and
// "1.2.3-beta.4" for the fourth Alpha or Beta build leading up to 1.2.3.
type Version struct {
	N       [3]int
	Channel int
	Build   int // 0 on Mainline
}

func number(s string) (int, bool) {
	if len(s) == 0 || len(s) > 5 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Parse reads a version. Anything else ("dev") is not a version, and such
// builds neither offer nor accept updates.
func Parse(v string) (Version, bool) {
	core, pre, isPre := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-")
	out := Version{Channel: Mainline}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	for i, p := range parts {
		x, ok := number(p)
		if !ok {
			return Version{}, false
		}
		out.N[i] = x
	}
	if isPre {
		name, build, _ := strings.Cut(pre, ".")
		switch name {
		case "alpha":
			out.Channel = Alpha
		case "beta":
			out.Channel = Beta
		default:
			return Version{}, false
		}
		b, ok := number(build)
		if !ok || b < 1 {
			return Version{}, false
		}
		out.Build = b
	}
	return out, true
}

// compare orders versions: by number first, then Alpha before Beta before
// the finished version of the same number, then by build.
func compare(a, b Version) int {
	for i := range a.N {
		if a.N[i] != b.N[i] {
			return a.N[i] - b.N[i]
		}
	}
	if a.Channel != b.Channel {
		return a.Channel - b.Channel
	}
	return a.Build - b.Build
}

// Newer reports whether version a is newer than version b.
func Newer(a, b string) bool {
	x, ok1 := Parse(a)
	y, ok2 := Parse(b)
	return ok1 && ok2 && compare(x, y) > 0
}

// Allowed reports whether a PC running version from may be moved to version
// to. Moving to something newer is always allowed. Moving back is allowed
// only to leave Alpha or Beta for a more settled channel, and never to a
// version from before channels existed; a Mainline PC is never moved back.
// A PC from before channels existed cannot read an Alpha or Beta version at
// all, so it must reach a current Mainline version first.
func Allowed(from, to string) bool {
	f, ok1 := Parse(from)
	t, ok2 := Parse(to)
	floor, _ := Parse(Floor)
	if !ok1 || !ok2 {
		return false
	}
	if t.Channel != Mainline && compare(f, floor) < 0 {
		return false
	}
	if compare(t, f) > 0 {
		return true
	}
	return t.Channel > f.Channel && compare(t, floor) >= 0
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

// releaseDoc is the part of GitHub's description of a release used here.
type releaseDoc struct {
	Tag    string `json:"tag_name"`
	Draft  bool   `json:"draft"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func fromDoc(doc releaseDoc) (Release, error) {
	r := Release{Version: strings.TrimPrefix(doc.Tag, "v")}
	if _, ok := Parse(r.Version); !ok || doc.Draft {
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

func release(ctx context.Context, path string) (Release, error) {
	data, err := get(ctx, apiBase+path, 4<<20)
	if errors.Is(err, errNotFound) {
		return Release{}, ErrNoRelease
	}
	if err != nil {
		return Release{}, err
	}
	var doc releaseDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Release{}, errors.New("GitHub's answer could not be read")
	}
	return fromDoc(doc)
}

// Latest returns the newest published version a PC on the given channel
// should run: the newest Mainline version, or for Beta and Alpha the newest
// of that channel and the more settled ones.
func Latest(ctx context.Context, channel int) (Release, error) {
	if channel >= Mainline {
		// GitHub's "latest" never names an Alpha or Beta version, which
		// are published as pre-releases.
		return release(ctx, "/releases/latest")
	}
	data, err := get(ctx, apiBase+"/releases?per_page=40", 16<<20)
	if errors.Is(err, errNotFound) {
		return Release{}, ErrNoRelease
	}
	if err != nil {
		return Release{}, err
	}
	var docs []releaseDoc
	if err := json.Unmarshal(data, &docs); err != nil {
		return Release{}, errors.New("GitHub's answer could not be read")
	}
	var best Release
	var bestV Version
	found := false
	for _, doc := range docs {
		r, err := fromDoc(doc)
		if err != nil {
			continue
		}
		v, _ := Parse(r.Version)
		if v.Channel < channel {
			continue
		}
		if !found || compare(v, bestV) > 0 {
			best, bestV, found = r, v, true
		}
	}
	if !found {
		return Release{}, ErrNoRelease
	}
	return best, nil
}

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

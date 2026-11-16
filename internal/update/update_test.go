package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{
		{"1.0.1", "1.0.0", true}, {"1.10.0", "1.9.9", true}, {"2.0.0", "1.99.99", true}, {"v1.2.3", "1.2.2", true},
		{"1.0.0", "1.0.0", false}, {"1.0.0", "1.0.1", false}, {"dev", "1.0.0", false}, {"1.0.0", "dev", false},
		{"1.0", "0.9.0", false}, {"1.0.0-beta", "0.9.0", false}, {"1.-1.0", "0.0.1", false},
		{"1.+1.0", "0.0.1", false}, {"1.0.0-gamma.1", "0.9.0", false}, {"1.0.0-alpha.0", "0.9.0", false},
		{"1.11.0-alpha.1", "1.10.0", true}, {"1.11.0-alpha.2", "1.11.0-alpha.1", true},
		{"1.11.0-beta.1", "1.11.0-alpha.9", true}, {"1.11.0", "1.11.0-beta.3", true},
		{"1.11.0-alpha.1", "1.11.0", false}, {"1.10.0", "1.11.0-alpha.1", false},
	} {
		if Newer(c.a, c.b) != c.newer {
			t.Errorf("Newer(%q, %q) should be %v", c.a, c.b, c.newer)
		}
	}
}

func TestAllowed(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"1.10.0", "1.10.1", true},                 // an ordinary update
		{"1.9.1", "1.10.0", true},                  // an old PC reaching Mainline
		{"1.10.0", "1.11.0-alpha.1", true},         // joining Alpha
		{"1.11.0-alpha.1", "1.11.0-alpha.2", true}, // the next Alpha
		{"1.11.0-alpha.2", "1.10.0", true},         // leaving Alpha for Mainline
		{"1.12.0-alpha.1", "1.11.0-beta.2", true},  // leaving Alpha for Beta
		{"1.11.0-beta.2", "1.10.3", true},          // leaving Beta for Mainline
		{"1.10.0", "1.10.0", false},                // nothing to do
		{"1.10.1", "1.10.0", false},                // Mainline never goes back
		{"1.11.0-alpha.2", "1.11.0-alpha.1", false},
		{"1.11.0-beta.1", "1.11.0-alpha.5", false}, // not towards a less settled channel
		{"1.11.0-alpha.1", "1.9.1", false},         // not to before channels existed
		{"1.9.1", "1.11.0-alpha.1", false},         // an old PC cannot read it
		{"dev", "1.10.0", false}, {"1.10.0", "dev", false},
	} {
		if Allowed(c.from, c.to) != c.ok {
			t.Errorf("Allowed(%q, %q) should be %v", c.from, c.to, c.ok)
		}
	}
	for _, ch := range []int{Alpha, Beta, Mainline} {
		if ChannelFromKey(ChannelKey(ch)) != ch {
			t.Errorf("channel %d does not survive being saved", ch)
		}
	}
	if ChannelFromKey("nonsense") != Mainline {
		t.Error("an unknown channel should mean Mainline")
	}
}

func TestSignatures(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	installer := []byte("pretend this is a setup program")
	sig := Sign(priv.Seed(), "1.2.3", installer)
	if err := verifyWith(key, "1.2.3", installer, sig+"\n"); err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for name, err := range map[string]error{
		"changed file":       verifyWith(key, "1.2.3", append([]byte("x"), installer...), sig),
		"claimed as newer":   verifyWith(key, "9.9.9", installer, sig),
		"someone else's key": verifyWith(base64.StdEncoding.EncodeToString(other), "1.2.3", installer, sig),
		"garbage signature":  verifyWith(key, "1.2.3", installer, "not a signature"),
		"empty signature":    verifyWith(key, "1.2.3", installer, ""),
		"not a version":      verifyWith(key, "dev", installer, Sign(priv.Seed(), "dev", installer)),
	} {
		if !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if verifyWith("", "1.2.3", installer, sig) == nil {
		t.Error("a build without a key accepted an update")
	}
	// The key actually built in must at least be well-formed.
	if b, err := base64.StdEncoding.DecodeString(PublicKey); err != nil || len(b) != ed25519.PublicKeySize {
		t.Error("the built-in public key is malformed")
	}
}

func TestReleases(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	installer := []byte(strings.Repeat("setup ", 5000))
	sig := Sign(priv.Seed(), "1.4.0", installer)
	listed := func(v string, draft bool, base string) string {
		d := "false"
		if draft {
			d = "true"
		}
		return `{"tag_name":"v` + v + `","draft":` + d + `,"assets":[
			{"name":"Deskbridge-Setup-` + v + `.exe","browser_download_url":"` + base + `/dl/v` + v + `/Deskbridge-Setup-` + v + `.exe"},
			{"name":"Deskbridge-Setup-` + v + `.exe.sig","browser_download_url":"` + base + `/dl/v` + v + `/Deskbridge-Setup-` + v + `.exe.sig"}]}`
	}
	list := []string{"1.4.0"}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/latest", "/api/releases/tags/v1.4.0":
			w.Write([]byte(`{"tag_name":"v1.4.0","assets":[
				{"name":"Deskbridge-Setup-1.4.0.exe","browser_download_url":"` + srv.URL + `/dl/v1.4.0/Deskbridge-Setup-1.4.0.exe"},
				{"name":"Deskbridge-Setup-1.4.0.exe.sig","browser_download_url":"` + srv.URL + `/dl/v1.4.0/Deskbridge-Setup-1.4.0.exe.sig"},
				{"name":"Deskbridge-Setup-1.4.0.exe","browser_download_url":"https://elsewhere.example/evil.exe"}]}`))
		case "/api/releases":
			var docs []string
			for _, v := range list {
				docs = append(docs, listed(v, false, srv.URL))
			}
			docs = append(docs, listed("9.9.9-alpha.1", true, srv.URL), // a draft
				`{"tag_name":"v9.9.8-alpha.1","assets":[]}`, // nothing signed
				`{"tag_name":"nightly","assets":[]}`)
			w.Write([]byte("[" + strings.Join(docs, ",") + "]"))
		case "/api/releases/tags/v1.5.0":
			w.Write([]byte(`{"tag_name":"v1.5.0","assets":[{"name":"Deskbridge-Setup-1.5.0.exe","browser_download_url":"` + srv.URL + `/dl/x"}]}`))
		case "/dl/v1.4.0/Deskbridge-Setup-1.4.0.exe":
			w.Write(installer)
		case "/dl/v1.4.0/Deskbridge-Setup-1.4.0.exe.sig":
			w.Write([]byte(sig + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	apiBase, downloadPrefix = srv.URL+"/api", srv.URL+"/dl/"
	ctx := context.Background()

	rel, err := Latest(ctx, Mainline)
	if err != nil || rel.Version != "1.4.0" || !strings.HasPrefix(rel.installerURL, srv.URL) {
		t.Fatal(rel, err)
	}
	// Download verifies against the built-in key, which is not this test's.
	if _, _, err := Download(ctx, rel); !errors.Is(err, ErrBadSignature) {
		t.Fatal("a release signed with the wrong key was accepted:", err)
	}
	if verifyWith(base64.StdEncoding.EncodeToString(pub), rel.Version, installer, sig) != nil {
		t.Fatal("test release does not verify with its own key")
	}
	if _, err := ByVersion(ctx, "1.5.0"); err == nil {
		t.Fatal("a release without a signature file was accepted")
	}
	if _, err := ByVersion(ctx, "7.7.7"); !errors.Is(err, ErrNoRelease) {
		t.Fatal("missing release:", err)
	}

	// Channels: each takes the newest of itself and the more settled ones.
	for _, c := range []struct {
		list    []string
		channel int
		want    string
	}{
		{[]string{"1.10.0", "1.11.0-alpha.1", "1.11.0-alpha.2", "1.9.1"}, Alpha, "1.11.0-alpha.2"},
		{[]string{"1.10.0", "1.11.0-alpha.2"}, Beta, "1.10.0"},
		{[]string{"1.11.0-beta.1", "1.10.0", "1.11.0-alpha.7"}, Beta, "1.11.0-beta.1"},
		{[]string{"1.11.0-beta.1", "1.10.0", "1.11.0-alpha.7"}, Alpha, "1.11.0-beta.1"},
		{[]string{"1.11.0-alpha.3", "1.11.0", "1.10.0"}, Alpha, "1.11.0"},
		{[]string{"1.12.0-alpha.1", "1.11.0", "1.11.0-alpha.3"}, Alpha, "1.12.0-alpha.1"},
	} {
		list = c.list
		got, err := Latest(ctx, c.channel)
		if err != nil || got.Version != c.want {
			t.Errorf("%s of %v: got %q (%v), want %q", ChannelName(c.channel), c.list, got.Version, err, c.want)
		}
	}
	list = []string{"1.11.0-alpha.1"}
	if _, err := Latest(ctx, Beta); !errors.Is(err, ErrNoRelease) {
		t.Error("Beta was offered an Alpha version:", err)
	}
}

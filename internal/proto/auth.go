package proto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"time"
)

// DeriveKey stretches the shared password. Do it once, not per connection.
func DeriveKey(password string) []byte {
	k, err := pbkdf2.Key(sha256.New, password, []byte("deskbridge-v1"), 200000, 32)
	if err != nil {
		panic(err)
	}
	return k
}

// NewServerTLS makes a throwaway self-signed certificate. The certificate is
// not what authenticates the server: the password proof below is bound to the
// TLS session, so a man in the middle cannot relay it.
func NewServerTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "deskbridge"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func proof(key []byte, role string, ekm []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(role))
	m.Write(ekm)
	return m.Sum(nil)
}

var ErrAuth = errors.New("wrong password")

// ErrNoMaintenance means the server turned a maintenance connection down
// in the way a server from before version 1.5.0 turns anything down that it
// does not recognise: it is either that old, or the password is wrong.
var ErrNoMaintenance = errors.New("the server does not take maintenance connections")

// A connection is made for one of two purposes. A client one shows and
// controls the screen, and takes the place of any client already connected.
// A maintenance one only asks the server's version and can hand it an
// update; it disturbs nobody.
const (
	RoleClient      = "client"
	RoleMaintenance = "maintenance"
)

// refusal is what a server answers to a wrong password. Servers before
// 1.5.0 answered with zeros, which is how a newer client tells them apart.
var refusal = append([]byte("deskbridge: wrong password"), make([]byte, 6)...)

// ServerHandshake checks the other side's password proof and says which
// kind of connection it asked for.
func ServerHandshake(nc net.Conn, cfg *tls.Config, key []byte) (*tls.Conn, string, error) {
	tc := tls.Server(nc, cfg)
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		return nil, "", err
	}
	cs := tc.ConnectionState()
	ekm, err := cs.ExportKeyingMaterial("deskbridge-auth", nil, 32)
	if err != nil {
		return nil, "", err
	}
	got := make([]byte, 32)
	if _, err := io.ReadFull(tc, got); err != nil {
		return nil, "", err
	}
	role := ""
	for _, r := range []string{RoleClient, RoleMaintenance} {
		if hmac.Equal(got, proof(key, r, ekm)) {
			role = r
		}
	}
	if role == "" {
		time.Sleep(time.Second)
		tc.Write(refusal)
		return nil, "", ErrAuth
	}
	if _, err := tc.Write(proof(key, "server", ekm)); err != nil {
		return nil, "", err
	}
	tc.SetDeadline(time.Time{})
	return tc, role, nil
}

// ClientHandshake connects as a client: see ClientHandshakeAs.
func ClientHandshake(nc net.Conn, key []byte) (*tls.Conn, error) {
	return ClientHandshakeAs(nc, key, RoleClient)
}

// ClientHandshakeAs proves knowledge of the password for the given kind of
// connection and checks that the server knows it too.
func ClientHandshakeAs(nc net.Conn, key []byte, role string) (*tls.Conn, error) {
	tc := tls.Client(nc, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	cs := tc.ConnectionState()
	ekm, err := cs.ExportKeyingMaterial("deskbridge-auth", nil, 32)
	if err != nil {
		return nil, err
	}
	if _, err := tc.Write(proof(key, role, ekm)); err != nil {
		return nil, err
	}
	got := make([]byte, 32)
	if _, err := io.ReadFull(tc, got); err != nil {
		return nil, ErrAuth
	}
	if !hmac.Equal(got, proof(key, "server", ekm)) {
		if role == RoleMaintenance && !hmac.Equal(got, refusal) {
			return nil, ErrNoMaintenance
		}
		return nil, ErrAuth
	}
	tc.SetDeadline(time.Time{})
	return tc, nil
}

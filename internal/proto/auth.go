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

func ServerHandshake(nc net.Conn, cfg *tls.Config, key []byte) (*tls.Conn, error) {
	tc := tls.Server(nc, cfg)
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	cs := tc.ConnectionState()
	ekm, err := cs.ExportKeyingMaterial("deskbridge-auth", nil, 32)
	if err != nil {
		return nil, err
	}
	got := make([]byte, 32)
	if _, err := io.ReadFull(tc, got); err != nil {
		return nil, err
	}
	if !hmac.Equal(got, proof(key, "client", ekm)) {
		time.Sleep(time.Second)
		tc.Write(make([]byte, 32))
		return nil, ErrAuth
	}
	if _, err := tc.Write(proof(key, "server", ekm)); err != nil {
		return nil, err
	}
	tc.SetDeadline(time.Time{})
	return tc, nil
}

func ClientHandshake(nc net.Conn, key []byte) (*tls.Conn, error) {
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
	if _, err := tc.Write(proof(key, "client", ekm)); err != nil {
		return nil, err
	}
	got := make([]byte, 32)
	if _, err := io.ReadFull(tc, got); err != nil {
		return nil, ErrAuth
	}
	if !hmac.Equal(got, proof(key, "server", ekm)) {
		return nil, ErrAuth
	}
	tc.SetDeadline(time.Time{})
	return tc, nil
}

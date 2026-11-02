// Package pki generates the minimal certificate set an embedded control plane
// needs: one CA, an apiserver serving cert, a service-account signing key and
// client certs for admin, the controllers and each node.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	Dir  string
}

const validity = 10 * 365 * 24 * time.Hour

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func newKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

func writePEM(path, typ string, b []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), mode)
}

func writeKey(path string, k crypto.Signer) error {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	return writePEM(path, "PRIVATE KEY", der, 0o600)
}

func readKey(path string) (crypto.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("bad pem: " + path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	return k.(crypto.Signer), nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("bad pem: " + path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

// LoadOrCreateCA loads dir/ca.{crt,key} or creates them.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	crt, key := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if c, err := readCert(crt); err == nil {
		k, err := readKey(key)
		if err != nil {
			return nil, err
		}
		return &CA{Cert: c, Key: k, Dir: dir}, nil
	}
	k, err := newKey()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "kvz-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		return nil, err
	}
	if err := writePEM(crt, "CERTIFICATE", der, 0o644); err != nil {
		return nil, err
	}
	if err := writeKey(key, k); err != nil {
		return nil, err
	}
	c, _ := x509.ParseCertificate(der)
	return &CA{Cert: c, Key: k, Dir: dir}, nil
}

// Issue writes dir/<name>.{crt,key} unless they already exist.
func (ca *CA) Issue(name, cn string, orgs []string, dns []string, ips []net.IP, server bool) (crtPath, keyPath string, err error) {
	crtPath, keyPath = filepath.Join(ca.Dir, name+".crt"), filepath.Join(ca.Dir, name+".key")
	if _, err := os.Stat(crtPath); err == nil {
		return crtPath, keyPath, nil
	}
	k, err := newKey()
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn, Organization: orgs},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	if server {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, k.Public(), ca.Key)
	if err != nil {
		return "", "", err
	}
	if err := writePEM(crtPath, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", err
	}
	return crtPath, keyPath, writeKey(keyPath, k)
}

// ServiceAccountKey writes dir/sa.{key,pub}.
func ServiceAccountKey(dir string) (keyPath, pubPath string, err error) {
	keyPath, pubPath = filepath.Join(dir, "sa.key"), filepath.Join(dir, "sa.pub")
	if _, err := os.Stat(keyPath); err == nil {
		return keyPath, pubPath, nil
	}
	k, err := newKey()
	if err != nil {
		return "", "", err
	}
	if err := writeKey(keyPath, k); err != nil {
		return "", "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		return "", "", err
	}
	return keyPath, pubPath, writePEM(pubPath, "PUBLIC KEY", pub, 0o644)
}

// WriteKubeconfig writes a kubeconfig embedding CA + client cert data.
func WriteKubeconfig(path, server, caPath, crtPath, keyPath, user string) error {
	enc := func(p string) (string, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		return b64(b), nil
	}
	ca, err := enc(caPath)
	if err != nil {
		return err
	}
	crt, err := enc(crtPath)
	if err != nil {
		return err
	}
	key, err := enc(keyPath)
	if err != nil {
		return err
	}
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: kvz
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
contexts:
- name: kvz
  context: {cluster: kvz, user: %s}
current-context: kvz
`, server, ca, user, crt, key, user)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(cfg), 0o600)
}

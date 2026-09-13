package agentfw

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// MITM mints ephemeral leaf certs signed by a local CA so the proxy can
// terminate agent TLS and scan decrypted response bodies.
type MITM struct {
	ca      *x509.Certificate
	caKey   crypto.Signer
	caPEM   []byte
	leaves  sync.Map // host -> *tls.Certificate
	minting sync.Map // host -> *sync.Mutex (singleflight per host)
}

// LoadMITM reads a CA cert + key PEM pair from disk.
func LoadMITM(certPath, keyPath string) (*MITM, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("mitm: read ca cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("mitm: read ca key: %w", err)
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return nil, fmt.Errorf("mitm: ca cert not PEM")
	}
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse ca cert: %w", err)
	}
	kblk, _ := pem.Decode(keyPEM)
	if kblk == nil {
		return nil, fmt.Errorf("mitm: ca key not PEM")
	}
	key, err := parseAnyPrivateKey(kblk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse ca key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("mitm: ca key is not a signer")
	}
	return &MITM{ca: ca, caKey: signer, caPEM: certPEM}, nil
}

// TLSConfig returns a tls.Config that mints certs on the fly per SNI.
func (m *MITM) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := hello.ServerName
			if host == "" {
				return nil, fmt.Errorf("mitm: missing SNI")
			}
			return m.leafFor(host)
		},
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
}

func (m *MITM) leafFor(host string) (*tls.Certificate, error) {
	if v, ok := m.leaves.Load(host); ok {
		return v.(*tls.Certificate), nil
	}
	muI, _ := m.minting.LoadOrStore(host, &sync.Mutex{})
	mu := muI.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := m.leaves.Load(host); ok {
		return v.(*tls.Certificate), nil
	}
	leaf, err := m.mint(host)
	if err != nil {
		return nil, err
	}
	m.leaves.Store(host, leaf)
	return leaf, nil
}

func (m *MITM) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, key.Public(), m.caKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, m.ca.Raw}, PrivateKey: key, Leaf: tmpl}, nil
}

// GenerateCA creates a new self-signed CA valid for 10 years and writes
// ca.crt + ca.key PEM files into outDir.
// GenerateCAPEM mints a fresh MITM CA and returns PEM-encoded ca.crt / ca.key.
func GenerateCAPEM() (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "agentfw MITM CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func GenerateCA(outDir string) (certPath, keyPath string, err error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", "", err
	}
	certPEM, keyPEM, err := GenerateCAPEM()
	if err != nil {
		return "", "", err
	}
	certPath = outDir + "/ca.crt"
	keyPath = outDir + "/ca.key"
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

// MatchesBypass returns true if host matches any suffix in bypass.
func MatchesBypass(host string, bypass []string) bool {
	// strip :port
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, suf := range bypass {
		suf = strings.TrimPrefix(suf, ".")
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}

func parseAnyPrivateKey(der []byte) (crypto.PrivateKey, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	return x509.ParseECPrivateKey(der)
}

// ponytail: unbounded leaf cache, add LRU if agents fan out to thousands of hosts.
// ponytail: no CA rotation, regenerate CA + redeploy when needed.

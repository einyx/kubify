package agentfw

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCAPEM(t *testing.T) {
	certPEM, keyPEM, err := GenerateCAPEM()
	if err != nil {
		t.Fatalf("GenerateCAPEM: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("ca.crt is not a PEM CERTIFICATE")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse ca.crt: %v", err)
	}
	if !cert.IsCA || cert.Subject.CommonName != "agentfw MITM CA" {
		t.Fatalf("certificate is not the expected CA (cn=%q isCA=%v)", cert.Subject.CommonName, cert.IsCA)
	}

	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil || kblock.Type != "PRIVATE KEY" {
		t.Fatalf("ca.key is not a PKCS#8 PRIVATE KEY")
	}
	if _, err := x509.ParsePKCS8PrivateKey(kblock.Bytes); err != nil {
		t.Fatalf("parse ca.key: %v", err)
	}

	// The CA must be loadable by the proxy itself.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMITM(certPath, keyPath); err != nil {
		t.Fatalf("LoadMITM on generated CA: %v", err)
	}
}

func TestGenerateCAWritesFiles(t *testing.T) {
	certPath, keyPath, err := GenerateCA(t.TempDir())
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if filepath.Base(certPath) != "ca.crt" || filepath.Base(keyPath) != "ca.key" {
		t.Fatalf("unexpected paths %q %q", certPath, keyPath)
	}
	if _, err := LoadMITM(certPath, keyPath); err != nil {
		t.Fatalf("LoadMITM: %v", err)
	}
}

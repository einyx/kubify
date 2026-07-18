package agentfw

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"os"
)

// Signer signs audit event JSON with Ed25519 for tamper-evident logs.
type Signer struct {
	priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// NewSigner loads the key from keyPath, or generates and saves a new key pair.
func NewSigner(keyPath string) (*Signer, error) {
	if b, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(b)
		if block != nil && len(block.Bytes) == ed25519.PrivateKeySize {
			priv := ed25519.PrivateKey(block.Bytes)
			return &Signer{priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	s := &Signer{priv: priv, Pub: pub}
	_ = s.save(keyPath) // best-effort; if we can't persist, we still work
	return s, nil
}

func (s *Signer) save(keyPath string) error {
	block := &pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: []byte(s.priv)}
	return os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600)
}

// Sign returns a base64-encoded Ed25519 signature over data.
func (s *Signer) Sign(data []byte) string {
	sig := ed25519.Sign(s.priv, data)
	return base64.StdEncoding.EncodeToString(sig)
}

// Verify checks a base64 signature against data using the public key.
func Verify(pub ed25519.PublicKey, data []byte, sig64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sig64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, data, sig)
}

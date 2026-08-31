// Package sign provides Ed25519 detached signatures over rendered PDFs —
// lightweight tamper evidence without PKI. The service holds one keypair;
// each artifact's signature is stored in its job row and returned in the
// X-Pdf-Signature header, verifiable against GET /v1/signing-key.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

type Signer struct {
	priv ed25519.PrivateKey
}

// Load reads the 32-byte Ed25519 seed at path, generating and persisting
// one (0600) on first boot.
func Load(path string) (*Signer, error) {
	seed, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			return nil, fmt.Errorf("persist signing key: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key %s: want %d-byte seed, got %d bytes",
			path, ed25519.SeedSize, len(seed))
	}
	return &Signer{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// Sign returns the base64 detached signature over the given bytes.
func (s *Signer) Sign(data []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, data))
}

// PublicKey returns the base64 public key for external verification.
func (s *Signer) PublicKey() string {
	return base64.StdEncoding.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

// Verify checks a base64 signature over data against a base64 public key.
func Verify(publicKeyB64 string, data []byte, signatureB64 string) (bool, error) {
	pub, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return false, fmt.Errorf("public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("public key: want %d bytes, got %d", ed25519.PublicKeySize, len(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return false, fmt.Errorf("signature: %w", err)
	}
	return ed25519.Verify(ed25519.PublicKey(pub), data, sig), nil
}

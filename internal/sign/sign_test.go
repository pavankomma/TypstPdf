package sign

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSignVerifyRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("first load should generate a key: %v", err)
	}
	doc := []byte("%PDF-1.7 pretend document")
	sig := s.Sign(doc)

	ok, err := Verify(s.PublicKey(), doc, sig)
	if err != nil || !ok {
		t.Fatalf("valid signature should verify, got %v, %v", ok, err)
	}

	tampered := append([]byte{}, doc...)
	tampered[0] = 'X'
	ok, err = Verify(s.PublicKey(), tampered, sig)
	if err != nil || ok {
		t.Fatalf("tampered document must not verify, got %v, %v", ok, err)
	}
}

func TestLoadIsStableAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	s1, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path) // same seed → same identity
	if err != nil {
		t.Fatal(err)
	}
	if s1.PublicKey() != s2.PublicKey() {
		t.Fatal("reloading the seed must yield the same public key")
	}
}

func TestLoadRejectsCorruptSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a truncated seed must be rejected, not silently regenerated")
	}
}

func TestVerifyRejectsMalformedInputs(t *testing.T) {
	if _, err := Verify("not-base64!!!", []byte("x"), "sig"); err == nil {
		t.Fatal("malformed public key must error")
	}
	s, _ := Load(filepath.Join(t.TempDir(), "k"))
	if _, err := Verify(s.PublicKey(), []byte("x"), "not-base64!!!"); err == nil {
		t.Fatal("malformed signature must error")
	}
	if ok, err := Verify(s.PublicKey()[:8], []byte("x"), s.Sign([]byte("x"))); ok || err == nil {
		t.Fatal("truncated public key must error")
	}
}

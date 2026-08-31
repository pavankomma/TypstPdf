// Package sign is a stand-in for Zerodha's jpdfsigner — a Java HTTP
// service wrapping OpenPDF that applies real PKCS#7 digital signatures.
//
// Running a JVM is out of scope for a sample, so this "signer" appends a
// signature trailer (a SHA-256 digest over the document, tagged with a
// mock certificate identity) after the PDF's %%EOF marker. PDF readers
// ignore trailing bytes, so the file stays a valid, openable PDF while
// carrying a verifiable digest. Swap this package for an HTTP call to a
// real signing service to make it production-shaped.
package sign

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"time"

	"github.com/example/contract-notes-pipeline/internal/store"
)

type Signer struct {
	Identity string // e.g. "CN=Sample Broking Ltd, O=Demo"
	Store    *store.Store
}

// Sign reads the generated PDF, appends the mock signature trailer, and
// stores the result under signed/.
func (s *Signer) Sign(clientID, pdfKey string) (string, error) {
	pdf, err := s.Store.Get(pdfKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(pdf)
	trailer := fmt.Sprintf(
		"\n%%%%MockSignature\n%%%% Identity: %s\n%%%% SHA-256: %x\n%%%% SignedAt: %s\n",
		s.Identity, digest, time.Now().UTC().Format(time.RFC3339),
	)
	signed := append(pdf, []byte(trailer)...)

	key := filepath.Join("signed", clientID+".pdf")
	if _, err := s.Store.Put(key, signed); err != nil {
		return "", err
	}
	return key, nil
}

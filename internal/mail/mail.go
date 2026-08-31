// Package mail is the delivery stage. Zerodha runs a self-hosted Haraka
// SMTP cluster and their smtppool Go library to blast out 1.5M emails;
// this sample composes real RFC 5322 MIME messages with the signed PDF
// attached and drops them as .eml files into an outbox directory —
// exactly what would be handed to an SMTP connection pool.
package mail

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/contract-notes-pipeline/internal/store"
)

type Mailer struct {
	From   string
	Outbox string // directory that stands in for the SMTP hand-off
	Store  *store.Store
}

// Send builds the MIME message for one client and writes it to the outbox.
func (m *Mailer) Send(clientID, name, email, signedKey string) (string, error) {
	pdf, err := m.Store.Get(signedKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.Outbox, 0o755); err != nil {
		return "", err
	}

	boundary := "==contract-note-" + clientID
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s <%s>\r\n", name, email)
	fmt.Fprintf(&b, "Subject: Contract note for %s\r\n", time.Now().Format("02 Jan 2006"))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", boundary)
	fmt.Fprintf(&b, "Dear %s,\r\n\r\nPlease find attached your digitally signed contract note.\r\n\r\n(Sample pipeline output — not a real financial document.)\r\n\r\n", name)

	fmt.Fprintf(&b, "--%s\r\nContent-Type: application/pdf\r\n", boundary)
	fmt.Fprintf(&b, "Content-Disposition: attachment; filename=%q\r\n", clientID+".pdf")
	fmt.Fprintf(&b, "Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString(pdf)
	for i := 0; i < len(enc); i += 76 {
		end := min(i+76, len(enc))
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)

	path := filepath.Join(m.Outbox, clientID+".eml")
	return path, os.WriteFile(path, []byte(b.String()), 0o644)
}

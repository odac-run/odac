package smtp

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"odac/internal/mail/address"
)

// dsnReport is what a bounce says about one failed recipient.
type dsnReport struct {
	arrival    time.Time
	diagnostic string
	now        time.Time
	original   []byte
	recipient  string
	sender     string
	status     string
}

// maxDSNHeaderBytes caps the original header block quoted in a bounce.
const maxDSNHeaderBytes = 64 * 1024

// buildDSN renders an RFC 3464 delivery status notification: a readable
// explanation, the machine-readable status, and the original message's headers
// so a mail client can thread the report with what was sent.
func buildDSN(r dsnReport) []byte {
	domain := address.Domain(r.sender)
	boundary := "odac-dsn-" + randomToken()
	diagnostic := oneLine(r.diagnostic)

	var b bytes.Buffer
	fmt.Fprintf(&b, "From: Mail Delivery System <MAILER-DAEMON@%s>\r\n", domain)
	fmt.Fprintf(&b, "To: <%s>\r\n", r.sender)
	b.WriteString("Subject: Undelivered Mail Returned to Sender\r\n")
	fmt.Fprintf(&b, "Date: %s\r\n", r.now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", randomToken(), domain)
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/report; report-type=delivery-status; boundary=\"%s\"\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n", boundary)
	fmt.Fprintf(&b, "Your message to <%s> could not be delivered.\r\n\r\n%s\r\n\r\n", oneLine(r.recipient), diagnostic)

	fmt.Fprintf(&b, "--%s\r\nContent-Type: message/delivery-status\r\n\r\n", boundary)
	fmt.Fprintf(&b, "Reporting-MTA: dns; mail.%s\r\n", domain)
	fmt.Fprintf(&b, "Arrival-Date: %s\r\n\r\n", r.arrival.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Final-Recipient: rfc822; %s\r\n", oneLine(r.recipient))
	b.WriteString("Action: failed\r\n")
	fmt.Fprintf(&b, "Status: %s\r\n", r.status)
	fmt.Fprintf(&b, "Diagnostic-Code: smtp; %s\r\n", diagnostic)
	fmt.Fprintf(&b, "Last-Attempt-Date: %s\r\n\r\n", r.now.Format(time.RFC1123Z))

	if headers := headerBlock(r.original); len(headers) > 0 {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/rfc822-headers\r\n\r\n", boundary)
		b.Write(headers)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// headerBlock returns the message's header section with CRLF line endings.
func headerBlock(msg []byte) []byte {
	end := bytes.Index(msg, []byte("\r\n\r\n"))
	if lf := bytes.Index(msg, []byte("\n\n")); lf >= 0 && (end < 0 || lf < end) {
		end = lf
	}
	if end < 0 {
		end = len(msg)
	}
	block := msg[:min(end, maxDSNHeaderBytes)]
	block = bytes.ReplaceAll(block, []byte("\r\n"), []byte("\n"))
	block = bytes.ReplaceAll(block, []byte("\n"), []byte("\r\n"))
	if len(block) > 0 && !bytes.HasSuffix(block, []byte("\r\n")) {
		block = append(block, '\r', '\n')
	}
	return block
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func randomToken() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

package imap

import (
	"database/sql"
	"io"
	"net"
	"strings"
	"testing"

	"odac/internal/mail/blob"
	"odac/internal/mail/storage"
)

// attachmentMsg is a multipart/mixed message carrying a real attachment, the
// shape that used to reach the mailbox with the attachment silently dropped.
var attachmentMsg = strings.ReplaceAll(`From: Sender <s@example.com>
To: Rcpt <r@example.com>
Subject: Invoice
Date: Tue, 18 Aug 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/plain; charset="UTF-8"

See the invoice attached.
--MIX
Content-Type: application/pdf; name="invoice.pdf"
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQKJUdPT0Q=
--MIX--
`, "\n", "\r\n")

// capture runs fn against a connection wired to an in-memory pipe and returns
// everything the server wrote.
func capture(t *testing.T, blobs *blob.Store, fn func(c *Connection)) string {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(client)
		done <- string(data)
	}()

	c := &Connection{conn: server, blobs: blobs}
	fn(c)
	server.Close()
	return <-done
}

// rawRow stores msg in a fresh blob store and returns the row referencing it.
func rawRow(t *testing.T, raw string) (*blob.Store, *storage.MessageRow) {
	t.Helper()
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("blob.NewStore: %v", err)
	}
	ref, err := blobs.Put([]byte(raw))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return blobs, &storage.MessageRow{
		UID:    7,
		RawRef: sql.NullString{String: ref, Valid: true},
		Text:   sql.NullString{String: "See the invoice attached.", Valid: true},
	}
}

// payload strips the IMAP literal header from a single BODY[...] response.
func payload(t *testing.T, out string) string {
	t.Helper()
	idx := strings.Index(out, "}\r\n")
	if idx < 0 {
		t.Fatalf("malformed response: %q", out)
	}
	return strings.TrimSuffix(out[idx+3:], " ")
}

func TestFetchAttachmentPartFromRawMessage(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[2]", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != "JVBERi0xLjQKJUdPT0Q=" {
		t.Errorf("BODY[2] = %q, want the attachment bytes as transmitted", got)
	}

	out = capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[1]", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != "See the invoice attached." {
		t.Errorf("BODY[1] = %q", got)
	}
}

func TestBodyStructureAnnouncesAttachment(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodyStructure(msg, newRawView(c, msg))
	})
	if !strings.Contains(out, `("ATTACHMENT" ("FILENAME" "invoice.pdf"))`) {
		t.Errorf("BODYSTRUCTURE does not announce the attachment:\n%s", out)
	}
	if !strings.Contains(out, `"APPLICATION" "PDF"`) {
		t.Errorf("BODYSTRUCTURE missing attachment media type:\n%s", out)
	}
	if !strings.Contains(out, `"BASE64"`) {
		t.Errorf("BODYSTRUCTURE missing transfer encoding:\n%s", out)
	}
}

func TestFetchWholeMessageIsVerbatim(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[]", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != attachmentMsg {
		t.Errorf("BODY[] is not byte-identical to the delivered message")
	}
}

func TestFetchHeaderFieldsFromRawMessage(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[HEADER.FIELDS (SUBJECT FROM)]", msg, newRawView(c, msg))
	})
	got := payload(t, out)
	if !strings.Contains(got, "Subject: Invoice") || !strings.Contains(got, "From: Sender <s@example.com>") {
		t.Errorf("HEADER.FIELDS = %q", got)
	}
	if strings.Contains(got, "To:") {
		t.Errorf("HEADER.FIELDS leaked an unrequested header: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Errorf("HEADER.FIELDS must end with a blank line, got %q", got)
	}
}

func TestFetchHeaderFieldsNot(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[HEADER.FIELDS.NOT (SUBJECT)]", msg, newRawView(c, msg))
	})
	got := payload(t, out)
	if strings.Contains(got, "Subject:") {
		t.Errorf("HEADER.FIELDS.NOT kept the excluded header: %q", got)
	}
	if !strings.Contains(got, "From: Sender <s@example.com>") {
		t.Errorf("HEADER.FIELDS.NOT dropped a header it should keep: %q", got)
	}
}

func TestPartialRangeAppliesToRawSection(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[2]<4.6>", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != "Ri0xLj" {
		t.Errorf("BODY[2]<4.6> = %q, want %q", got, "Ri0xLj")
	}
	if !strings.Contains(out, "BODY[2]<4>") {
		t.Errorf("partial response must echo the origin octet:\n%s", out)
	}
}

// A row written before raw storage existed still has to render.
func TestLegacyRowFallsBackToSynthesizedBody(t *testing.T) {
	msg := &storage.MessageRow{
		UID:  3,
		Text: sql.NullString{String: "legacy body", Valid: true},
	}
	out := capture(t, nil, func(c *Connection) {
		c.writeBodySection("BODY[1]", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != "legacy body" {
		t.Errorf("legacy BODY[1] = %q", got)
	}

	out = capture(t, nil, func(c *Connection) {
		c.writeBodyStructure(msg, newRawView(c, msg))
	})
	if !strings.Contains(out, `"TEXT" "PLAIN"`) {
		t.Errorf("legacy BODYSTRUCTURE = %q", out)
	}
}

// A row whose blob has gone missing must degrade, not fail the fetch.
func TestMissingBlobFallsBackToSynthesizedBody(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)
	if err := blobs.Delete(msg.RawRef.String); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[1]", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != "See the invoice attached." {
		t.Errorf("fallback BODY[1] = %q", got)
	}
}

func TestUnknownSectionIsEmptyNotSubstituted(t *testing.T) {
	blobs, msg := rawRow(t, attachmentMsg)

	out := capture(t, blobs, func(c *Connection) {
		c.writeBodySection("BODY[9]", msg, newRawView(c, msg))
	})
	if got := payload(t, out); got != "" {
		t.Errorf("BODY[9] = %q, want empty for a part that does not exist", got)
	}
}

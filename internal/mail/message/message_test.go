package message

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMessage_SimpleText(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: Hello\r\nMessage-ID: <abc@example.com>\r\n\r\nThis is the body.\r\n"

	msg := Parse([]byte(raw))

	if msg.Subject != "Hello" {
		t.Errorf("subject = %q, want Hello", msg.Subject)
	}
	if msg.MessageID != "<abc@example.com>" {
		t.Errorf("MessageID = %q", msg.MessageID)
	}
	if msg.Text != "This is the body.\r\n" {
		t.Errorf("text = %q", msg.Text)
	}
	if !strings.Contains(msg.From, "sender@example.com") {
		t.Errorf("from should contain sender address, got %s", msg.From)
	}
	if !strings.Contains(msg.To, "rcpt@example.com") {
		t.Errorf("to should contain recipient address, got %s", msg.To)
	}
	if msg.HeaderLinesJSON == "" || msg.HeaderLinesJSON == "null" {
		t.Error("HeaderLinesJSON should not be empty")
	}
}

func TestParseMessage_HTMLContent(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: HTML\r\nContent-Type: text/html\r\n\r\n<h1>Hello</h1>\r\n"

	msg := Parse([]byte(raw))

	if msg.HTML != "<h1>Hello</h1>\r\n" {
		t.Errorf("html = %q", msg.HTML)
	}
	if msg.Text != "" {
		t.Errorf("text should be empty for html-only, got %q", msg.Text)
	}
}

func TestParseMessage_MultipartAlternative(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Multi\r\nContent-Type: multipart/alternative; boundary=\"boundary123\"\r\n\r\n--boundary123\r\nContent-Type: text/plain\r\n\r\nPlain text\r\n--boundary123\r\nContent-Type: text/html\r\n\r\n<p>HTML text</p>\r\n--boundary123--\r\n"

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.Text, "Plain text") {
		t.Errorf("text = %q, should contain 'Plain text'", msg.Text)
	}
	if !strings.Contains(msg.HTML, "<p>HTML text</p>") {
		t.Errorf("html = %q, should contain '<p>HTML text</p>'", msg.HTML)
	}
}

// This is the exact format of the SendTestEmail message that failed
func TestParseMessage_MultipartNoQuoteBoundary(t *testing.T) {
	raw := "To: mail@emre.red\r\nSubject: Test\r\nFrom: Test <test@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative;boundary=ste69db6978124e8\r\n\r\n--ste69db6978124e8\r\nContent-Type: text/plain;charset=utf-8\r\n\r\nPlain text content\r\n--ste69db6978124e8\r\nContent-Type: text/html;charset=utf-8\r\n\r\n<b>HTML content</b>\r\n--ste69db6978124e8--\r\n"

	msg := Parse([]byte(raw))

	if msg.Text == "" {
		t.Errorf("text should not be empty, multipart with unquoted boundary should be parsed")
	}
	if !strings.Contains(msg.Text, "Plain text content") {
		t.Errorf("text = %q, should contain 'Plain text content'", msg.Text)
	}
	if msg.HTML == "" {
		t.Errorf("html should not be empty")
	}
	if !strings.Contains(msg.HTML, "<b>HTML content</b>") {
		t.Errorf("html = %q, should contain '<b>HTML content</b>'", msg.HTML)
	}
	if msg.Subject != "Test" {
		t.Errorf("subject = %q", msg.Subject)
	}
}

func TestParseMessage_MixedCaseBoundary(t *testing.T) {
	raw := "From: sender@java-server.com\r\nTo: rcpt@example.com\r\nSubject: Mixed Case\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"----=_Part_5048008_556565411.1776343091886\"\r\n\r\n------=_Part_5048008_556565411.1776343091886\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nPlain text version\r\n------=_Part_5048008_556565411.1776343091886\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<html><body><p>HTML version</p></body></html>\r\n------=_Part_5048008_556565411.1776343091886--\r\n"

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.Text, "Plain text version") {
		t.Errorf("text = %q, should contain 'Plain text version'", msg.Text)
	}
	if !strings.Contains(msg.HTML, "<html><body><p>HTML version</p></body></html>") {
		t.Errorf("html = %q, should contain HTML content", msg.HTML)
	}
}

func TestParseMessage_NestedMultipartMixedCase(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Nested\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed;\r\n boundary=\"----=_Outer_123\"\r\n\r\n------=_Outer_123\r\nContent-Type: multipart/alternative;\r\n boundary=\"----=_Inner_456\"\r\n\r\n------=_Inner_456\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nNested plain\r\n------=_Inner_456\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<p>Nested HTML</p>\r\n------=_Inner_456--\r\n------=_Outer_123--\r\n"

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.Text, "Nested plain") {
		t.Errorf("text = %q, should contain 'Nested plain'", msg.Text)
	}
	if !strings.Contains(msg.HTML, "<p>Nested HTML</p>") {
		t.Errorf("html = %q, should contain nested HTML", msg.HTML)
	}
}

func TestParseMessage_DisplayNameFrom(t *testing.T) {
	raw := "From: \"John Doe\" <john@example.com>\r\nTo: jane@example.com\r\nSubject: Test\r\n\r\nBody\r\n"

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.From, "john@example.com") {
		t.Errorf("from should contain address, got %s", msg.From)
	}
	if !strings.Contains(msg.From, "John Doe") {
		t.Errorf("from should contain display name, got %s", msg.From)
	}
}

func TestFormatAddressJSON_PlainEmail(t *testing.T) {
	result := formatAddressJSON("user@example.com")
	if !strings.Contains(result, "user@example.com") {
		t.Errorf("should contain email, got %s", result)
	}
}

func TestFormatAddressJSON_WithDisplayName(t *testing.T) {
	result := formatAddressJSON("\"Test User\" <test@example.com>")
	if !strings.Contains(result, "test@example.com") {
		t.Errorf("should contain email, got %s", result)
	}
	if !strings.Contains(result, "Test User") {
		t.Errorf("should contain name, got %s", result)
	}
}

func TestFormatAddressJSON_Empty(t *testing.T) {
	result := formatAddressJSON("")
	if result != `{"value":[]}` {
		t.Errorf("empty should return empty value array, got %s", result)
	}
}

func TestParseMessage_QuotedPrintableSinglePart(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: QP Test\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<html><body>Hello=20World=20=20Test</body></html>\r\n"

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.HTML, "Hello World  Test") {
		t.Errorf("QP single-part html not decoded, got %q", msg.HTML)
	}
	if strings.Contains(msg.HTML, "=20") {
		t.Errorf("html still contains =20 artifacts: %q", msg.HTML)
	}
}

func TestParseMessage_QuotedPrintableMultipart(t *testing.T) {
	raw := "From: sender@test.com\r\nTo: rcpt@test.com\r\nSubject: QP Multi\r\nContent-Type: multipart/alternative; boundary=\"qpbound\"\r\n\r\n--qpbound\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello=20World\r\n--qpbound\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>Hello=20World=3D=3DTest</p>\r\n--qpbound--\r\n"

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.Text, "Hello World") {
		t.Errorf("QP text part not decoded, got %q", msg.Text)
	}
	if !strings.Contains(msg.HTML, "Hello World==Test") {
		t.Errorf("QP html part not decoded, got %q", msg.HTML)
	}
	if strings.Contains(msg.HTML, "=20") || strings.Contains(msg.HTML, "=3D") {
		t.Errorf("html still contains QP artifacts: %q", msg.HTML)
	}
}

func TestParseMessage_Base64SinglePart(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: B64\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nPGh0bWw+PGJvZHk+SGVsbG8gV29ybGQ8L2JvZHk+PC9odG1sPg==\r\n"

	msg := Parse([]byte(raw))

	if msg.HTML != "<html><body>Hello World</body></html>" {
		t.Errorf("base64 html not decoded, got %q", msg.HTML)
	}
}

func TestParseMessage_Base64Multipart(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: B64 Multi\r\nContent-Type: multipart/alternative; boundary=\"b64bound\"\r\n\r\n--b64bound\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8gV29ybGQ=\r\n--b64bound\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nPHA+SGVsbG8gV29ybGQ8L3A+\r\n--b64bound--\r\n"

	msg := Parse([]byte(raw))

	if msg.Text != "Hello World" {
		t.Errorf("base64 text not decoded, got %q", msg.Text)
	}
	if msg.HTML != "<p>Hello World</p>" {
		t.Errorf("base64 html not decoded, got %q", msg.HTML)
	}
}

func TestParseMessage_QuotedPrintableSoftLineBreak(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Soft Break\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nThis is a long line that has been =\r\nwrapped using soft line breaks.\r\n"

	msg := Parse([]byte(raw))

	expected := "This is a long line that has been wrapped using soft line breaks.\r\n"
	if msg.Text != expected {
		t.Errorf("QP soft line break not handled, got %q, want %q", msg.Text, expected)
	}
}

// A text/plain file sent as an attachment used to overwrite the message body,
// because the old walk matched Content-Type and never read Content-Disposition.
func TestParseMessage_TextAttachmentDoesNotOverwriteBody(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@b.com
To: c@d.com
Subject: Notes attached
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/plain; charset="UTF-8"

Real message body.
--MIX
Content-Type: text/plain; name="notes.txt"
Content-Disposition: attachment; filename="notes.txt"

ATTACHMENT CONTENT
--MIX--
`, "\n", "\r\n")

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.Text, "Real message body.") {
		t.Errorf("text = %q, want the message body", msg.Text)
	}
	if strings.Contains(msg.Text, "ATTACHMENT CONTENT") {
		t.Errorf("attachment content leaked into the body: %q", msg.Text)
	}
}

// A binary attachment must never be mistaken for message content.
func TestParseMessage_BinaryAttachmentStaysOutOfBody(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@b.com
To: c@d.com
Subject: Invoice
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/html; charset="UTF-8"

<p>See attached.</p>
--MIX
Content-Type: application/pdf; name="invoice.pdf"
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQKJUdPT0Q=
--MIX--
`, "\n", "\r\n")

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.HTML, "<p>See attached.</p>") {
		t.Errorf("html = %q", msg.HTML)
	}
	if strings.Contains(msg.Text, "PDF") || strings.Contains(msg.HTML, "JVBERi") {
		t.Errorf("attachment bytes leaked into the body: text=%q html=%q", msg.Text, msg.HTML)
	}
}

// A forwarded message is an attachment; its body is not this message's body.
func TestParseMessage_ForwardedMessageStaysOutOfBody(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@b.com
To: c@d.com
Subject: Fwd
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/plain

Forwarding this to you.
--MIX
Content-Type: message/rfc822

From: original@example.com
Subject: Original
Content-Type: text/plain

INNER MESSAGE BODY
--MIX--
`, "\n", "\r\n")

	msg := Parse([]byte(raw))

	if !strings.Contains(msg.Text, "Forwarding this to you.") {
		t.Errorf("text = %q", msg.Text)
	}
	if strings.Contains(msg.Text, "INNER MESSAGE BODY") {
		t.Errorf("forwarded body leaked into the outer body: %q", msg.Text)
	}
}

func TestParseMessage_AttachmentIndex(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@b.com
To: c@d.com
Subject: Invoice
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/plain

body
--MIX
Content-Type: application/pdf; name="invoice.pdf"
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64
Content-ID: <abc-123@example.com>

aGVsbG8gd29ybGQ=
--MIX--
`, "\n", "\r\n")

	msg := Parse([]byte(raw))

	var metas []attachmentMeta
	if err := json.Unmarshal([]byte(msg.AttachmentsJSON), &metas); err != nil {
		t.Fatalf("attachments JSON invalid: %v (%s)", err, msg.AttachmentsJSON)
	}
	if len(metas) != 1 {
		t.Fatalf("attachments = %d, want 1", len(metas))
	}
	a := metas[0]

	if a.Filename != "invoice.pdf" || a.ContentType != "application/pdf" {
		t.Errorf("filename/type = %q/%q", a.Filename, a.ContentType)
	}
	if a.PartID != "2" {
		t.Errorf("partId = %q, want 2", a.PartID)
	}
	if a.ContentID != "<abc-123@example.com>" || a.CID != "abc-123@example.com" {
		t.Errorf("contentId/cid = %q/%q", a.ContentID, a.CID)
	}
	if a.Encoding != "base64" {
		t.Errorf("encoding = %q", a.Encoding)
	}
	// Size describes the decoded file, Length the encoded bytes in the message.
	if a.Size != len("hello world") {
		t.Errorf("size = %d, want %d", a.Size, len("hello world"))
	}
	if a.Length != len("aGVsbG8gd29ybGQ=") {
		t.Errorf("length = %d, want %d", a.Length, len("aGVsbG8gd29ybGQ="))
	}
	// md5("hello world")
	if a.Checksum != "5eb63bbbe01eeed093cb22bb8f5acdc3" {
		t.Errorf("checksum = %q", a.Checksum)
	}

	// The recorded range must cut the encoded attachment out of the raw message.
	if got := string([]byte(raw)[a.Offset : a.Offset+a.Length]); got != "aGVsbG8gd29ybGQ=" {
		t.Errorf("offset/length do not address the attachment: %q", got)
	}

	// Attachment bytes must not be inlined; that was the old format's mistake.
	if strings.Contains(msg.AttachmentsJSON, "aGVsbG8") || strings.Contains(msg.AttachmentsJSON, `"data"`) {
		t.Errorf("attachment content leaked into the index: %s", msg.AttachmentsJSON)
	}
}

func TestParseMessage_NoAttachmentsLeavesIndexEmpty(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Plain\r\n\r\njust text\r\n"

	msg := Parse([]byte(raw))

	if msg.AttachmentsJSON != "" {
		t.Errorf("attachments = %q, want empty for a message with none", msg.AttachmentsJSON)
	}
}

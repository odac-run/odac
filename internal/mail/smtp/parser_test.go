package smtp

import (
	"strings"
	"testing"
)

func TestParseMessage_SimpleText(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: Hello\r\nMessage-ID: <abc@example.com>\r\n\r\nThis is the body.\r\n"

	msg := parseMessage([]byte(raw))

	if msg.subject != "Hello" {
		t.Errorf("subject = %q, want Hello", msg.subject)
	}
	if msg.messageID != "<abc@example.com>" {
		t.Errorf("messageID = %q", msg.messageID)
	}
	if msg.text != "This is the body.\r\n" {
		t.Errorf("text = %q", msg.text)
	}
	if !strings.Contains(msg.from, "sender@example.com") {
		t.Errorf("from should contain sender address, got %s", msg.from)
	}
	if !strings.Contains(msg.to, "rcpt@example.com") {
		t.Errorf("to should contain recipient address, got %s", msg.to)
	}
	if msg.headerLinesJSON == "" || msg.headerLinesJSON == "null" {
		t.Error("headerLinesJSON should not be empty")
	}
}

func TestParseMessage_HTMLContent(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: HTML\r\nContent-Type: text/html\r\n\r\n<h1>Hello</h1>\r\n"

	msg := parseMessage([]byte(raw))

	if msg.html != "<h1>Hello</h1>\r\n" {
		t.Errorf("html = %q", msg.html)
	}
	if msg.text != "" {
		t.Errorf("text should be empty for html-only, got %q", msg.text)
	}
}

func TestParseMessage_MultipartAlternative(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Multi\r\nContent-Type: multipart/alternative; boundary=\"boundary123\"\r\n\r\n--boundary123\r\nContent-Type: text/plain\r\n\r\nPlain text\r\n--boundary123\r\nContent-Type: text/html\r\n\r\n<p>HTML text</p>\r\n--boundary123--\r\n"

	msg := parseMessage([]byte(raw))

	if !strings.Contains(msg.text, "Plain text") {
		t.Errorf("text = %q, should contain 'Plain text'", msg.text)
	}
	if !strings.Contains(msg.html, "<p>HTML text</p>") {
		t.Errorf("html = %q, should contain '<p>HTML text</p>'", msg.html)
	}
}

// This is the exact format of the SendTestEmail message that failed
func TestParseMessage_MultipartNoQuoteBoundary(t *testing.T) {
	raw := "To: mail@emre.red\r\nSubject: Test\r\nFrom: Test <test@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative;boundary=ste69db6978124e8\r\n\r\n--ste69db6978124e8\r\nContent-Type: text/plain;charset=utf-8\r\n\r\nPlain text content\r\n--ste69db6978124e8\r\nContent-Type: text/html;charset=utf-8\r\n\r\n<b>HTML content</b>\r\n--ste69db6978124e8--\r\n"

	msg := parseMessage([]byte(raw))

	if msg.text == "" {
		t.Errorf("text should not be empty, multipart with unquoted boundary should be parsed")
	}
	if !strings.Contains(msg.text, "Plain text content") {
		t.Errorf("text = %q, should contain 'Plain text content'", msg.text)
	}
	if msg.html == "" {
		t.Errorf("html should not be empty")
	}
	if !strings.Contains(msg.html, "<b>HTML content</b>") {
		t.Errorf("html = %q, should contain '<b>HTML content</b>'", msg.html)
	}
	if msg.subject != "Test" {
		t.Errorf("subject = %q", msg.subject)
	}
}

func TestParseMessage_MixedCaseBoundary(t *testing.T) {
	raw := "From: sender@java-server.com\r\nTo: rcpt@example.com\r\nSubject: Mixed Case\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"----=_Part_5048008_556565411.1776343091886\"\r\n\r\n------=_Part_5048008_556565411.1776343091886\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nPlain text version\r\n------=_Part_5048008_556565411.1776343091886\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<html><body><p>HTML version</p></body></html>\r\n------=_Part_5048008_556565411.1776343091886--\r\n"

	msg := parseMessage([]byte(raw))

	if !strings.Contains(msg.text, "Plain text version") {
		t.Errorf("text = %q, should contain 'Plain text version'", msg.text)
	}
	if !strings.Contains(msg.html, "<html><body><p>HTML version</p></body></html>") {
		t.Errorf("html = %q, should contain HTML content", msg.html)
	}
}

func TestParseMessage_NestedMultipartMixedCase(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Nested\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed;\r\n boundary=\"----=_Outer_123\"\r\n\r\n------=_Outer_123\r\nContent-Type: multipart/alternative;\r\n boundary=\"----=_Inner_456\"\r\n\r\n------=_Inner_456\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nNested plain\r\n------=_Inner_456\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<p>Nested HTML</p>\r\n------=_Inner_456--\r\n------=_Outer_123--\r\n"

	msg := parseMessage([]byte(raw))

	if !strings.Contains(msg.text, "Nested plain") {
		t.Errorf("text = %q, should contain 'Nested plain'", msg.text)
	}
	if !strings.Contains(msg.html, "<p>Nested HTML</p>") {
		t.Errorf("html = %q, should contain nested HTML", msg.html)
	}
}

func TestParseMessage_DisplayNameFrom(t *testing.T) {
	raw := "From: \"John Doe\" <john@example.com>\r\nTo: jane@example.com\r\nSubject: Test\r\n\r\nBody\r\n"

	msg := parseMessage([]byte(raw))

	if !strings.Contains(msg.from, "john@example.com") {
		t.Errorf("from should contain address, got %s", msg.from)
	}
	if !strings.Contains(msg.from, "John Doe") {
		t.Errorf("from should contain display name, got %s", msg.from)
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

	msg := parseMessage([]byte(raw))

	if !strings.Contains(msg.html, "Hello World  Test") {
		t.Errorf("QP single-part html not decoded, got %q", msg.html)
	}
	if strings.Contains(msg.html, "=20") {
		t.Errorf("html still contains =20 artifacts: %q", msg.html)
	}
}

func TestParseMessage_QuotedPrintableMultipart(t *testing.T) {
	raw := "From: sender@test.com\r\nTo: rcpt@test.com\r\nSubject: QP Multi\r\nContent-Type: multipart/alternative; boundary=\"qpbound\"\r\n\r\n--qpbound\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello=20World\r\n--qpbound\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>Hello=20World=3D=3DTest</p>\r\n--qpbound--\r\n"

	msg := parseMessage([]byte(raw))

	if !strings.Contains(msg.text, "Hello World") {
		t.Errorf("QP text part not decoded, got %q", msg.text)
	}
	if !strings.Contains(msg.html, "Hello World==Test") {
		t.Errorf("QP html part not decoded, got %q", msg.html)
	}
	if strings.Contains(msg.html, "=20") || strings.Contains(msg.html, "=3D") {
		t.Errorf("html still contains QP artifacts: %q", msg.html)
	}
}

func TestParseMessage_Base64SinglePart(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: B64\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nPGh0bWw+PGJvZHk+SGVsbG8gV29ybGQ8L2JvZHk+PC9odG1sPg==\r\n"

	msg := parseMessage([]byte(raw))

	if msg.html != "<html><body>Hello World</body></html>" {
		t.Errorf("base64 html not decoded, got %q", msg.html)
	}
}

func TestParseMessage_Base64Multipart(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: B64 Multi\r\nContent-Type: multipart/alternative; boundary=\"b64bound\"\r\n\r\n--b64bound\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8gV29ybGQ=\r\n--b64bound\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nPHA+SGVsbG8gV29ybGQ8L3A+\r\n--b64bound--\r\n"

	msg := parseMessage([]byte(raw))

	if msg.text != "Hello World" {
		t.Errorf("base64 text not decoded, got %q", msg.text)
	}
	if msg.html != "<p>Hello World</p>" {
		t.Errorf("base64 html not decoded, got %q", msg.html)
	}
}

func TestParseMessage_QuotedPrintableSoftLineBreak(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: Soft Break\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nThis is a long line that has been =\r\nwrapped using soft line breaks.\r\n"

	msg := parseMessage([]byte(raw))

	expected := "This is a long line that has been wrapped using soft line breaks.\r\n"
	if msg.text != expected {
		t.Errorf("QP soft line break not handled, got %q, want %q", msg.text, expected)
	}
}

func TestDecodeBody_7bit(t *testing.T) {
	body := "Hello World"
	if result := decodeBody(body, "7bit"); result != body {
		t.Errorf("7bit should pass through unchanged, got %q", result)
	}
	if result := decodeBody(body, ""); result != body {
		t.Errorf("empty encoding should pass through unchanged, got %q", result)
	}
}

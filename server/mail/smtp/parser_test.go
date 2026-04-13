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

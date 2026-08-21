package mimetree

import (
	"strings"
	"testing"
)

// crlf converts a readable test fixture into wire form.
func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

const mixedMsg = `From: Sender <s@example.com>
To: Rcpt <r@example.com>
Subject: Report
Date: Tue, 18 Aug 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="OUTER"

preamble text, ignored
--OUTER
Content-Type: multipart/alternative; boundary="OUTER-INNER"

--OUTER-INNER
Content-Type: text/plain; charset="UTF-8"
Content-Transfer-Encoding: 7bit

plain body
--OUTER-INNER
Content-Type: text/html; charset="UTF-8"
Content-Transfer-Encoding: 7bit

<p>html body</p>
--OUTER-INNER--
--OUTER
Content-Type: application/gzip; name="report.xml.gz"
Content-Disposition: attachment; filename="report.xml.gz"
Content-Transfer-Encoding: base64
Content-ID: <abc@example.com>

H4sIAAAAAAAA
--OUTER--
`

func TestParseMixedTree(t *testing.T) {
	raw := crlf(mixedMsg)
	root := Parse(raw)

	if root.MediaType() != "multipart/mixed" {
		t.Fatalf("root type = %s, want multipart/mixed", root.MediaType())
	}
	if len(root.Children) != 2 {
		t.Fatalf("root children = %d, want 2", len(root.Children))
	}

	alt := root.Children[0]
	if alt.MediaType() != "multipart/alternative" {
		t.Fatalf("part 1 = %s, want multipart/alternative", alt.MediaType())
	}
	// The inner boundary extends the outer one; a prefix match would have
	// collapsed the two levels into one.
	if len(alt.Children) != 2 {
		t.Fatalf("alternative children = %d, want 2", len(alt.Children))
	}
	if got := string(alt.Children[0].RawBody(raw)); got != "plain body" {
		t.Errorf("part 1.1 body = %q, want %q", got, "plain body")
	}
	if got := string(alt.Children[1].RawBody(raw)); got != "<p>html body</p>" {
		t.Errorf("part 1.2 body = %q, want %q", got, "<p>html body</p>")
	}

	att := root.Children[1]
	if att.MediaType() != "application/gzip" {
		t.Fatalf("part 2 = %s, want application/gzip", att.MediaType())
	}
	if !att.IsAttachment() {
		t.Error("part 2 should be an attachment")
	}
	if att.Filename() != "report.xml.gz" {
		t.Errorf("filename = %q, want report.xml.gz", att.Filename())
	}
	if got := string(att.RawBody(raw)); got != "H4sIAAAAAAAA" {
		t.Errorf("attachment raw body = %q", got)
	}
}

func TestResolveSections(t *testing.T) {
	raw := crlf(mixedMsg)
	root := Parse(raw)

	cases := []struct {
		spec string
		want string
	}{
		{"1.1", "plain body"},
		{"1.2", "<p>html body</p>"},
		{"2", "H4sIAAAAAAAA"},
	}
	for _, tc := range cases {
		p, kind, ok := root.Resolve(tc.spec)
		if !ok {
			t.Fatalf("Resolve(%q) failed", tc.spec)
		}
		s, e := p.Range(kind)
		if got := string(raw[s:e]); got != tc.want {
			t.Errorf("BODY[%s] = %q, want %q", tc.spec, got, tc.want)
		}
	}

	// BODY[] is the entire message including its headers.
	p, kind, ok := root.Resolve("")
	if !ok {
		t.Fatal("Resolve(\"\") failed")
	}
	s, e := p.Range(kind)
	if s != 0 || e != len(raw) {
		t.Errorf("BODY[] range = [%d,%d), want [0,%d)", s, e, len(raw))
	}

	// BODY[2.MIME] is the attachment's own headers, which is where a client
	// reads the filename from.
	p, kind, ok = root.Resolve("2.MIME")
	if !ok {
		t.Fatal("Resolve(2.MIME) failed")
	}
	s, e = p.Range(kind)
	if !strings.Contains(string(raw[s:e]), "filename=\"report.xml.gz\"") {
		t.Errorf("BODY[2.MIME] missing filename: %q", raw[s:e])
	}

	if _, _, ok := root.Resolve("9"); ok {
		t.Error("Resolve(9) should fail for a two-part message")
	}
}

func TestBodyStructureReportsAttachment(t *testing.T) {
	raw := crlf(mixedMsg)
	bs := Parse(raw).BodyStructure(raw)

	// The disposition field is what makes a client show a paperclip.
	if !strings.Contains(bs, `("ATTACHMENT" ("FILENAME" "report.xml.gz"))`) {
		t.Errorf("BODYSTRUCTURE missing attachment disposition:\n%s", bs)
	}
	if !strings.Contains(bs, `"APPLICATION" "GZIP"`) {
		t.Errorf("BODYSTRUCTURE missing attachment type:\n%s", bs)
	}
	if !strings.Contains(bs, `"MIXED"`) || !strings.Contains(bs, `"ALTERNATIVE"`) {
		t.Errorf("BODYSTRUCTURE missing multipart subtypes:\n%s", bs)
	}
}

func TestSinglePartMessage(t *testing.T) {
	raw := crlf("Subject: hi\nContent-Type: text/plain\n\nhello world\n")
	root := Parse(raw)

	if root.MediaType() != "text/plain" {
		t.Fatalf("type = %s", root.MediaType())
	}
	// RFC 3501: part 1 of a single-part message is the message body.
	p, kind, ok := root.Resolve("1")
	if !ok {
		t.Fatal("Resolve(1) failed")
	}
	s, e := p.Range(kind)
	if got := string(raw[s:e]); got != "hello world\r\n" {
		t.Errorf("BODY[1] = %q", got)
	}
}

func TestNoContentTypeDefaultsToTextPlain(t *testing.T) {
	root := Parse(crlf("Subject: bare\n\njust text\n"))
	if root.MediaType() != "text/plain" {
		t.Errorf("type = %s, want text/plain", root.MediaType())
	}
}

func TestBoundaryPrefixIsNotAMatch(t *testing.T) {
	msg := crlf(`Content-Type: multipart/mixed; boundary="B"

--BXX
not a real delimiter
--B
Content-Type: text/plain

only part
--B--
`)
	root := Parse(msg)
	if len(root.Children) != 1 {
		t.Fatalf("children = %d, want 1 (--BXX must not delimit)", len(root.Children))
	}
	if got := string(root.Children[0].RawBody(msg)); got != "only part" {
		t.Errorf("body = %q, want %q", got, "only part")
	}
}

func TestNestedMessageRFC822(t *testing.T) {
	msg := crlf(`Content-Type: multipart/mixed; boundary="B"

--B
Content-Type: text/plain

see attached
--B
Content-Type: message/rfc822

From: inner <i@example.com>
Subject: forwarded
Content-Type: text/plain

inner body
--B--
`)
	root := Parse(msg)
	if len(root.Children) != 2 {
		t.Fatalf("children = %d, want 2", len(root.Children))
	}
	fwd := root.Children[1]
	if !fwd.IsMessage() {
		t.Fatal("part 2 should embed a message")
	}
	if fwd.Nested.MediaType() != "text/plain" {
		t.Errorf("nested type = %s", fwd.Nested.MediaType())
	}

	// BODY[2.1] addresses the body of the embedded message.
	p, kind, ok := root.Resolve("2.1")
	if !ok {
		t.Fatal("Resolve(2.1) failed")
	}
	s, e := p.Range(kind)
	if got := string(msg[s:e]); got != "inner body" {
		t.Errorf("BODY[2.1] = %q, want %q", got, "inner body")
	}

	// The nested envelope is mandatory in a message/rfc822 BODYSTRUCTURE.
	bs := root.BodyStructure(msg)
	if !strings.Contains(bs, `"forwarded"`) {
		t.Errorf("BODYSTRUCTURE missing nested envelope subject:\n%s", bs)
	}
}

func TestDecodedBody(t *testing.T) {
	msg := crlf("Content-Type: text/plain\nContent-Transfer-Encoding: base64\n\naGVsbG8gd29ybGQ=\n")
	root := Parse(msg)
	if got := string(root.DecodedBody(msg)); got != "hello world" {
		t.Errorf("decoded = %q, want %q", got, "hello world")
	}
}

func TestUnterminatedMultipartKeepsLastPart(t *testing.T) {
	msg := crlf(`Content-Type: multipart/mixed; boundary="B"

--B
Content-Type: text/plain

truncated tail`)
	root := Parse(msg)
	if len(root.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(root.Children))
	}
	if got := string(root.Children[0].RawBody(msg)); got != "truncated tail" {
		t.Errorf("body = %q", got)
	}
}

func TestNonASCIIFilenameUsesLiteral(t *testing.T) {
	msg := crlf(`Content-Type: multipart/mixed; boundary="B"

--B
Content-Type: application/pdf
Content-Disposition: attachment; filename="rapor-ölçüm.pdf"

data
--B--
`)
	bs := Parse(msg).BodyStructure(msg)
	// A quoted string cannot carry 8-bit octets, so a literal must be used.
	if !strings.Contains(bs, "{18}\r\nrapor-ölçüm.pdf") {
		t.Errorf("expected literal for non-ASCII filename:\n%s", bs)
	}
}

func TestAttachmentsCarrySectionPaths(t *testing.T) {
	raw := crlf(mixedMsg)
	atts := Parse(raw).Attachments()

	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if atts[0].PartID != "2" {
		t.Errorf("partId = %q, want 2", atts[0].PartID)
	}

	// The recorded path must round-trip back through Resolve, otherwise a
	// stored partId cannot be used to fetch the file later.
	root := Parse(raw)
	p, kind, ok := root.Resolve(atts[0].PartID)
	if !ok {
		t.Fatalf("Resolve(%q) failed", atts[0].PartID)
	}
	s, e := p.Range(kind)
	if string(raw[s:e]) != "H4sIAAAAAAAA" {
		t.Errorf("partId %q does not address the attachment", atts[0].PartID)
	}
}

func TestAttachmentsInsideForwardedMessage(t *testing.T) {
	msg := crlf(`Content-Type: multipart/mixed; boundary="B"

--B
Content-Type: text/plain

fwd
--B
Content-Type: message/rfc822

From: i@example.com
Subject: original
Content-Type: multipart/mixed; boundary="C"

--C
Content-Type: text/plain

inner text
--C
Content-Type: image/png
Content-Disposition: attachment; filename="shot.png"

PNGDATA
--C--
--B--
`)
	atts := Parse(msg).Attachments()

	var paths []string
	for _, a := range atts {
		paths = append(paths, a.PartID)
	}
	// The forwarded message itself, then the image inside it.
	if len(paths) != 2 || paths[0] != "2" || paths[1] != "2.2" {
		t.Fatalf("paths = %v, want [2 2.2]", paths)
	}

	root := Parse(msg)
	p, kind, ok := root.Resolve("2.2")
	if !ok {
		t.Fatal("Resolve(2.2) failed")
	}
	s, e := p.Range(kind)
	if got := string(msg[s:e]); got != "PNGDATA" {
		t.Errorf("BODY[2.2] = %q, want PNGDATA", got)
	}
}

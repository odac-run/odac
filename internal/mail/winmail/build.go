package winmail

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"net/textproto"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"odac/internal/mail/tnef"
)

// maxNameBytes keeps a header carrying a file name under the 998 byte line
// limit of RFC 5322, even with every byte percent-encoded by RFC 2231.
const maxNameBytes = 255

// entity is one MIME entity to write: a leaf with an encoded body, or a
// multipart with children.
type entity struct {
	nl       string
	header   []string // field lines, without line endings
	body     []byte   // encoded body of a leaf
	subtype  string   // multipart subtype; children are set when it is
	boundary string
	children []*entity
}

func (e *entity) bytes() []byte {
	var b bytes.Buffer
	b.Grow(e.size())
	e.write(&b)
	return b.Bytes()
}

func (e *entity) size() int {
	n := len(e.body) + 256
	for _, c := range e.children {
		n += c.size()
	}
	return n
}

func (e *entity) write(b *bytes.Buffer) {
	for _, h := range e.header {
		b.WriteString(h)
		b.WriteString(e.nl)
	}
	if e.subtype == "" {
		b.WriteString(e.nl)
		b.Write(e.body)
		return
	}
	fmt.Fprintf(b, "Content-Type: multipart/%s; boundary=%q%s%s", e.subtype, e.boundary, e.nl, e.nl)
	for _, c := range e.children {
		b.WriteString("--" + e.boundary + e.nl)
		c.write(b)
	}
	b.WriteString("--" + e.boundary + "--" + e.nl)
}

// builder turns a decoded TNEF message into MIME entities.
//
// Nothing it writes can be mistaken for a delimiter of the message around
// it: every body is base64, whose alphabet has no '-', and every header line
// starts with a field name.
type builder struct {
	nl   string
	salt string
	seq  int
}

func (b *builder) boundary() string {
	if b.salt == "" {
		var r [12]byte
		_, _ = rand.Read(r[:])
		b.salt = hex.EncodeToString(r[:])
	}
	b.seq++
	return fmt.Sprintf("=_odac_%s_%d", b.salt, b.seq)
}

func (b *builder) multipart(subtype string, children []*entity) *entity {
	return &entity{nl: b.nl, subtype: subtype, boundary: b.boundary(), children: children}
}

func (b *builder) leaf(header []string, data []byte) *entity {
	header = append(header, "Content-Transfer-Encoding: base64")
	return &entity{nl: b.nl, header: header, body: encodeBase64(data, b.nl)}
}

// content is what replaces a winmail.dat: its body when withBody is set, then
// its attachments. It is nil when the stream held nothing to show, which is
// the common case of an RTF copy of a body the message already has.
func (b *builder) content(m *tnef.Message, withBody bool) *entity {
	var body *entity
	if withBody {
		body = b.body(m)
	}
	var inline, files []*entity
	for i, a := range m.Attachments {
		e := b.attachment(a, i+1)
		switch {
		case e == nil:
		case body != nil && len(m.HTML) > 0 && a.Hidden && a.ContentID != "":
			inline = append(inline, e)
		default:
			files = append(files, e)
		}
	}
	if withBody && body == nil && len(m.RTF) > 0 {
		// The body exists only as RTF, which no other client renders.
		files = append([]*entity{b.file("message.rtf", "application/rtf", "", false, m.RTF)}, files...)
	}
	if len(inline) > 0 {
		body = b.multipart("related", append([]*entity{body}, inline...))
	}
	parts := files
	if body != nil {
		parts = append([]*entity{body}, files...)
	}
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	}
	return b.multipart("mixed", parts)
}

func (b *builder) body(m *tnef.Message) *entity {
	var alt []*entity
	if m.Body != "" {
		alt = append(alt, b.leaf([]string{"Content-Type: text/plain; charset=utf-8"}, []byte(m.Body)))
	}
	if len(m.HTML) > 0 {
		cs := m.HTMLCharset
		if cs == "" {
			cs = "utf-8"
		}
		alt = append(alt, b.leaf([]string{"Content-Type: " + mime.FormatMediaType("text/html", map[string]string{"charset": cs})}, m.HTML))
	}
	switch len(alt) {
	case 0:
		return nil
	case 1:
		return alt[0]
	}
	return b.multipart("alternative", alt)
}

func (b *builder) attachment(a tnef.Attachment, n int) *entity {
	if a.Message != nil {
		return b.message(a)
	}
	if len(a.Data) == 0 && a.Name == "" {
		return nil
	}
	name := a.Name
	if name == "" {
		name = fmt.Sprintf("attachment-%d", n)
	}
	return b.file(name, mediaType(a), a.ContentID, a.Hidden && a.ContentID != "", a.Data)
}

func (b *builder) file(name, ct, cid string, inline bool, data []byte) *entity {
	name = truncate(name, maxNameBytes)
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	header := []string{
		"Content-Type: " + formatParam(ct, "name", name),
		"Content-Disposition: " + formatParam(disp, "filename", name),
	}
	if id := cleanCID(cid); id != "" {
		header = append(header, "Content-ID: <"+id+">")
	}
	return b.leaf(header, data)
}

// message renders an attached Outlook message as message/rfc822, built from
// the headers Outlook kept for it and its own converted content.
func (b *builder) message(a tnef.Attachment) *entity {
	m := a.Message
	var body bytes.Buffer
	for _, h := range transportHeaders(m, b.nl) {
		body.WriteString(h + b.nl)
	}
	body.WriteString("MIME-Version: 1.0" + b.nl)
	c := b.content(m, true)
	if c == nil {
		c = &entity{nl: b.nl, header: []string{"Content-Type: text/plain; charset=utf-8"}}
	}
	c.write(&body)

	name := a.Name
	if strings.HasSuffix(strings.ToLower(name), ".msg") {
		name = name[:len(name)-4]
	}
	if name == "" {
		name = "message"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".eml") {
		name = truncate(name, maxNameBytes-4) + ".eml"
	}
	return &entity{
		nl: b.nl,
		header: []string{
			"Content-Type: message/rfc822",
			"Content-Disposition: " + formatParam("attachment", "filename", name),
		},
		body: body.Bytes(),
	}
}

// keptHeaders are the fields of an attached message worth keeping. The rest
// describe a MIME structure the conversion no longer has, or the path it took.
var keptHeaders = []string{
	"Date", "From", "Sender", "Reply-To", "To", "Cc",
	"Subject", "Message-Id", "In-Reply-To", "References",
}

func transportHeaders(m *tnef.Message, nl string) []string {
	var h textproto.MIMEHeader
	if m.TransportHeaders != "" {
		r := textproto.NewReader(bufio.NewReader(strings.NewReader(m.TransportHeaders + "\r\n\r\n")))
		h, _ = r.ReadMIMEHeader()
	}
	var out []string
	hasSubject := false
	for _, name := range keptHeaders {
		for _, v := range h[textproto.CanonicalMIMEHeaderKey(name)] {
			// A raw 8-bit value cannot be re-encoded without knowing which
			// parts of it are addresses, so it is left out.
			if !printableASCII(v) || len(v) > 900 {
				continue
			}
			out = append(out, name+": "+v)
			hasSubject = hasSubject || name == "Subject"
		}
	}
	if !hasSubject && m.Subject != "" {
		s := mime.QEncoding.Encode("utf-8", truncate(m.Subject, 600))
		out = append(out, "Subject: "+strings.ReplaceAll(s, "?= =?", "?="+nl+" =?"))
	}
	return out
}

func mediaType(a tnef.Attachment) string {
	ct := a.ContentType
	if ct == "" {
		ct, _, _ = mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(a.Name)))
	}
	// A multipart or message type must not be base64 encoded (RFC 2046), and
	// the bytes are a file either way.
	if ct == "" || len(ct) > 127 || strings.HasPrefix(ct, "multipart/") || strings.HasPrefix(ct, "message/") {
		return "application/octet-stream"
	}
	return ct
}

func formatParam(value, param, arg string) string {
	if s := mime.FormatMediaType(value, map[string]string{param: arg}); s != "" {
		return s
	}
	return value
}

// cleanCID keeps the printable ASCII of a Content-ID, without the angle
// brackets that delimit it.
func cleanCID(s string) string {
	s = strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' || r == '<' || r == '>' {
			return -1
		}
		return r
	}, s)
	return truncate(s, 250)
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < ' ' && c != '\t') || c > '~' {
			return false
		}
	}
	return true
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func encodeBase64(data []byte, nl string) []byte {
	const chunk = 57 // 76 encoded characters per line
	lines := (len(data) + chunk - 1) / chunk
	out := make([]byte, 0, base64.StdEncoding.EncodedLen(len(data))+lines*len(nl))
	for len(data) > 0 {
		n := min(chunk, len(data))
		out = base64.StdEncoding.AppendEncode(out, data[:n])
		out = append(out, nl...)
		data = data[n:]
	}
	return out
}

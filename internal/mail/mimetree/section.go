package mimetree

import (
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strconv"
	"strings"
)

// Kind selects which byte range of a resolved part a section specifier names.
type Kind int

const (
	// KindMessage is a whole entity, headers and body together. Only BODY[]
	// selects it; a part addressed by number yields its body alone
	// (RFC 3501 §6.4.5).
	KindMessage Kind = iota
	// KindBody is the body of a part addressed by number.
	KindBody
	// KindHeader is the header block of a message entity.
	KindHeader
	// KindText is the body of a message entity.
	KindText
	// KindMIME is the MIME header block of a body part.
	KindMIME
)

// Resolve maps an IMAP section specifier ("", "1", "2.1", "2.MIME", "HEADER",
// "TEXT", "HEADER.FIELDS (...)") onto a part and the range kind it selects.
func (p *Part) Resolve(spec string) (*Part, Kind, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return p, KindMessage, true
	}

	// HEADER.FIELDS carries a parenthesized list; the numeric prefix is all
	// this function needs, so cut the list off before splitting on dots.
	head := spec
	if idx := strings.Index(head, "("); idx >= 0 {
		head = strings.TrimSpace(head[:idx])
	}

	fields := strings.Split(head, ".")
	cur := p
	i := 0
	for ; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			break
		}
		next := cur.child(n)
		if next == nil {
			return nil, KindMessage, false
		}
		cur = next
	}

	rest := strings.ToUpper(strings.Join(fields[i:], "."))
	switch {
	case rest == "":
		return cur, KindBody, true
	case rest == "MIME":
		// MIME is the only specifier that never crosses into an embedded
		// message: it names the headers of the part itself.
		if cur == p {
			return nil, KindMessage, false
		}
		return cur, KindMIME, true
	case rest == "HEADER" || strings.HasPrefix(rest, "HEADER.FIELDS"):
		return cur.messageEntity(), KindHeader, true
	case rest == "TEXT":
		return cur.messageEntity(), KindText, true
	}
	return nil, KindMessage, false
}

// messageEntity returns the entity whose headers HEADER/TEXT refer to. For a
// message/rfc822 part that is the embedded message, not the part wrapper.
func (p *Part) messageEntity() *Part {
	if p.Nested != nil {
		return p.Nested
	}
	return p
}

// child returns the numbered subpart, descending through an embedded message
// and treating a single-part message's body as part 1.
func (p *Part) child(n int) *Part {
	if n < 1 {
		return nil
	}
	if p.Type == "multipart" {
		if n <= len(p.Children) {
			return p.Children[n-1]
		}
		return nil
	}
	if p.Nested != nil {
		return p.Nested.child(n)
	}
	if n == 1 {
		return p
	}
	return nil
}

// Range returns the byte range in the original message for the given kind.
func (p *Part) Range(kind Kind) (int, int) {
	switch kind {
	case KindHeader, KindMIME:
		return p.HeaderStart, p.BodyStart
	case KindMessage:
		return p.HeaderStart, p.BodyEnd
	default:
		return p.BodyStart, p.BodyEnd
	}
}

// Size is the number of transferred (still encoded) octets in the part body.
func (p *Part) Size() int { return p.BodyEnd - p.BodyStart }

// Lines counts the body lines of the part, as required by the BODYSTRUCTURE
// text and message body types.
func (p *Part) Lines(raw []byte) int {
	if p.BodyEnd <= p.BodyStart || p.BodyEnd > len(raw) {
		return 0
	}
	body := raw[p.BodyStart:p.BodyEnd]
	n := 0
	for _, b := range body {
		if b == '\n' {
			n++
		}
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		n++
	}
	return n
}

// RawBody returns the part body exactly as transferred.
func (p *Part) RawBody(raw []byte) []byte {
	if p.BodyStart < 0 || p.BodyEnd > len(raw) || p.BodyEnd < p.BodyStart {
		return nil
	}
	return raw[p.BodyStart:p.BodyEnd]
}

// DecodedBody returns the part body with its Content-Transfer-Encoding undone.
// Undecodable content is returned as transferred, since a partially readable
// body beats none at all.
func (p *Part) DecodedBody(raw []byte) []byte {
	body := p.RawBody(raw)
	switch p.Encoding {
	case "base64":
		// Mail in the wild wraps base64 across lines and occasionally pads
		// badly; strip whitespace and fall back to a lenient decode.
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(body))
		if decoded, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
			return decoded
		}
		if decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(cleaned, "=")); err == nil {
			return decoded
		}
		return body
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(body))))
		if err != nil && len(decoded) == 0 {
			return body
		}
		return decoded
	default:
		return body
	}
}

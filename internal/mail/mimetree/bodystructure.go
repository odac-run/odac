package mimetree

import (
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"
)

// BodyStructure renders the RFC 3501 §7.4.2 BODYSTRUCTURE for the entity.
// The extension fields are emitted through body-fld-lang; clients accept an
// extension list truncated at the end, and location adds nothing useful here.
func (p *Part) BodyStructure(raw []byte) string {
	var sb strings.Builder
	p.writeStructure(&sb, raw)
	return sb.String()
}

func (p *Part) writeStructure(sb *strings.Builder, raw []byte) {
	switch {
	case p.IsMultipart():
		sb.WriteString("(")
		for _, c := range p.Children {
			c.writeStructure(sb, raw)
		}
		fmt.Fprintf(sb, " %s %s %s %s)",
			astring(strings.ToUpper(p.Subtype)),
			paramList(p.Params),
			dispField(p),
			langField(p))

	case p.IsMessage():
		fmt.Fprintf(sb, "(%s %s %s %s %s %s %d ",
			astring(strings.ToUpper(p.Type)),
			astring(strings.ToUpper(p.Subtype)),
			paramList(p.Params),
			astring(p.ContentID),
			astring(p.Description),
			astring(strings.ToUpper(p.Encoding)),
			p.Size())
		sb.WriteString(p.Nested.Envelope())
		sb.WriteString(" ")
		p.Nested.writeStructure(sb, raw)
		fmt.Fprintf(sb, " %d %s %s %s)", p.Lines(raw), nilOr(""), dispField(p), langField(p))

	case p.IsText():
		fmt.Fprintf(sb, "(%s %s %s %s %s %s %d %d %s %s %s)",
			astring("TEXT"),
			astring(strings.ToUpper(p.Subtype)),
			paramList(p.Params),
			astring(p.ContentID),
			astring(p.Description),
			astring(strings.ToUpper(p.Encoding)),
			p.Size(),
			p.Lines(raw),
			nilOr(""),
			dispField(p),
			langField(p))

	default:
		fmt.Fprintf(sb, "(%s %s %s %s %s %s %d %s %s %s)",
			astring(strings.ToUpper(p.Type)),
			astring(strings.ToUpper(p.Subtype)),
			paramList(p.Params),
			astring(p.ContentID),
			astring(p.Description),
			astring(strings.ToUpper(p.Encoding)),
			p.Size(),
			nilOr(""),
			dispField(p),
			langField(p))
	}
}

// Envelope renders the RFC 3501 ENVELOPE for a message entity:
// (date subject from sender reply-to to cc bcc in-reply-to message-id)
func (p *Part) Envelope() string {
	h := p.Header
	from := addrList(h.Get("From"))
	sender := addrList(h.Get("Sender"))
	replyTo := addrList(h.Get("Reply-To"))
	if sender == "NIL" {
		sender = from
	}
	if replyTo == "NIL" {
		replyTo = from
	}
	return fmt.Sprintf("(%s %s %s %s %s %s %s %s %s %s)",
		astring(envelopeDate(h.Get("Date"))),
		astring(h.Get("Subject")),
		from, sender, replyTo,
		addrList(h.Get("To")),
		addrList(h.Get("Cc")),
		addrList(h.Get("Bcc")),
		astring(h.Get("In-Reply-To")),
		astring(h.Get("Message-Id")))
}

// envelopeDate normalizes a Date header to the RFC 5322 form Apple Mail and
// iOS require; an unparsable date is passed through untouched.
func envelopeDate(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if t, err := mail.ParseDate(v); err == nil {
		return t.Format("Mon, 02 Jan 2006 15:04:05 -0700")
	}
	if t, err := time.Parse(time.RFC1123Z, v); err == nil {
		return t.Format("Mon, 02 Jan 2006 15:04:05 -0700")
	}
	return v
}

// addrList renders an address header as an IMAP address structure list.
func addrList(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "NIL"
	}
	addrs, err := mail.ParseAddressList(v)
	if err != nil || len(addrs) == 0 {
		return "NIL"
	}
	var sb strings.Builder
	sb.WriteString("(")
	n := 0
	for _, a := range addrs {
		at := strings.LastIndex(a.Address, "@")
		if at < 0 {
			continue
		}
		fmt.Fprintf(&sb, "(%s NIL %s %s)",
			astring(a.Name), astring(a.Address[:at]), astring(a.Address[at+1:]))
		n++
	}
	sb.WriteString(")")
	if n == 0 {
		return "NIL"
	}
	return sb.String()
}

// paramList renders a body-fld-param: ("KEY" "VALUE" ...) or NIL.
// Keys are sorted so the same message always yields the same response, which
// keeps client-side response caches from thrashing.
func paramList(params map[string]string) string {
	if len(params) == 0 {
		return "NIL"
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "boundary" || params[k] != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return "NIL"
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("(")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(" ")
		}
		fmt.Fprintf(&sb, "%s %s", astring(strings.ToUpper(k)), astring(params[k]))
	}
	sb.WriteString(")")
	return sb.String()
}

// dispField renders body-fld-dsp. This is the field that tells a client a part
// is an attachment and what to call it, so an empty disposition on a part that
// still carries a filename is reported as an attachment rather than as NIL.
func dispField(p *Part) string {
	disp := p.Disposition
	params := p.DispParams
	if disp == "" {
		if name := p.Params["name"]; name != "" && !p.IsText() {
			disp = "attachment"
			params = map[string]string{"filename": name}
		} else {
			return "NIL"
		}
	}
	return fmt.Sprintf("(%s %s)", astring(strings.ToUpper(disp)), paramList(params))
}

func langField(p *Part) string {
	return astring(strings.TrimSpace(p.Header.Get("Content-Language")))
}

func nilOr(s string) string { return astring(s) }

// astring renders a Go string as an IMAP astring: NIL when empty, a quoted
// string when it is plain 7-bit text, and a literal otherwise. Filenames and
// subjects routinely carry UTF-8 that a quoted string may not hold.
func astring(s string) string {
	if s == "" {
		return "NIL"
	}
	needsLiteral := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c == '\r' || c == '\n' || c == 0 {
			needsLiteral = true
			break
		}
	}
	if needsLiteral {
		return fmt.Sprintf("{%d}\r\n%s", len(s), s)
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(s[i])
	}
	sb.WriteByte('"')
	return sb.String()
}

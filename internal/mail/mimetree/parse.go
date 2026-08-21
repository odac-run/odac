// Package mimetree parses an RFC 5322 message into a MIME part tree that keeps
// byte offsets into the original message. Offsets are what let the IMAP layer
// answer BODY[2] by handing back the exact bytes the sender transmitted,
// instead of re-encoding a part and hoping clients agree with the result.
package mimetree

import (
	"bufio"
	"bytes"
	"mime"
	"net/textproto"
	"strings"
)

// MaxDepth caps nesting so a hostile message cannot drive unbounded recursion.
const MaxDepth = 20

// MaxParts caps the total number of parts extracted from one message.
const MaxParts = 1000

// Part is one MIME entity: the message itself, or a part nested inside it.
// All offsets are absolute indexes into the raw message passed to Parse.
type Part struct {
	Type    string // lowercased top-level type, e.g. "text"
	Subtype string // lowercased subtype, e.g. "plain"
	Params  map[string]string

	ContentID   string
	Description string
	Encoding    string // lowercased Content-Transfer-Encoding, "7bit" when absent
	Disposition string // lowercased Content-Disposition, empty when absent
	DispParams  map[string]string

	Header textproto.MIMEHeader

	HeaderStart int // first byte of this entity (its MIME headers)
	BodyStart   int // first byte of the body, just past the blank line
	BodyEnd     int // one past the last body byte

	Children []*Part // populated for multipart/*
	Nested   *Part   // populated for message/rfc822: the embedded message
}

// Parse builds the part tree for a complete RFC 5322 message.
func Parse(raw []byte) *Part {
	budget := MaxParts
	return parseEntity(raw, 0, len(raw), 0, &budget, false)
}

// IsMultipart reports whether the entity carries child parts.
func (p *Part) IsMultipart() bool { return p.Type == "multipart" && len(p.Children) > 0 }

// IsMessage reports whether the entity embeds another message.
func (p *Part) IsMessage() bool { return p.Nested != nil }

// IsText reports whether the entity is a text/* body.
func (p *Part) IsText() bool { return p.Type == "text" }

// MediaType returns the lowercased "type/subtype" string.
func (p *Part) MediaType() string { return p.Type + "/" + p.Subtype }

// Filename returns the part's declared filename, preferring the
// Content-Disposition filename over the legacy Content-Type name parameter.
func (p *Part) Filename() string {
	if v := p.DispParams["filename"]; v != "" {
		return v
	}
	return p.Params["name"]
}

// IsAttachment reports whether the part should be presented as an attachment
// rather than as message body. A part with an explicit attachment disposition
// always counts; so does any non-text, non-multipart part that is not an
// inline referenced resource.
func (p *Part) IsAttachment() bool {
	if p.Type == "multipart" {
		return false
	}
	switch p.Disposition {
	case "attachment":
		return true
	case "inline":
		// An inline part with a filename is still a file the user can save.
		return p.Filename() != "" && !p.IsText()
	}
	return !p.IsText() && p.Nested == nil
}

// parseEntity parses the entity occupying raw[start:end].
func parseEntity(raw []byte, start, end, depth int, budget *int, inDigest bool) *Part {
	p := &Part{
		HeaderStart: start,
		BodyStart:   end,
		BodyEnd:     end,
		Params:      map[string]string{},
		DispParams:  map[string]string{},
		Header:      textproto.MIMEHeader{},
		Encoding:    "7bit",
	}

	bodyStart := headerEnd(raw, start, end)
	p.BodyStart = bodyStart

	if bodyStart > start {
		block := raw[start:bodyStart]
		r := textproto.NewReader(bufio.NewReader(bytes.NewReader(block)))
		// A malformed header line yields a partial map plus an error; the
		// partial map is still the best available reading of the message.
		if h, err := r.ReadMIMEHeader(); h != nil {
			p.Header = h
			_ = err
		}
	}

	// RFC 2045 §5.2: a missing Content-Type defaults to text/plain, except
	// inside multipart/digest where it defaults to message/rfc822.
	defaultType := "text/plain"
	if inDigest {
		defaultType = "message/rfc822"
	}
	ct := p.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || ct == "" {
		mt, params = defaultType, map[string]string{}
		// Salvage a bare "type/subtype" from a header whose parameters are
		// malformed, rather than discarding the type along with them.
		if ct != "" {
			if bare := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]); strings.Contains(bare, "/") {
				mt = strings.ToLower(bare)
			}
		}
	}
	if slash := strings.Index(mt, "/"); slash > 0 {
		p.Type, p.Subtype = mt[:slash], mt[slash+1:]
	} else {
		p.Type, p.Subtype = "text", "plain"
	}
	for k, v := range params {
		p.Params[strings.ToLower(k)] = v
	}

	if enc := strings.TrimSpace(p.Header.Get("Content-Transfer-Encoding")); enc != "" {
		p.Encoding = strings.ToLower(enc)
	}
	p.ContentID = strings.TrimSpace(p.Header.Get("Content-Id"))
	p.Description = strings.TrimSpace(p.Header.Get("Content-Description"))

	if cd := p.Header.Get("Content-Disposition"); cd != "" {
		disp, dparams, derr := mime.ParseMediaType(cd)
		if derr != nil {
			disp = strings.ToLower(strings.TrimSpace(strings.SplitN(cd, ";", 2)[0]))
		}
		p.Disposition = strings.ToLower(disp)
		for k, v := range dparams {
			p.DispParams[strings.ToLower(k)] = v
		}
	}

	if depth >= MaxDepth || *budget <= 0 {
		return p
	}

	switch {
	case p.Type == "multipart":
		boundary := p.Params["boundary"]
		if boundary == "" {
			return p
		}
		digest := p.Subtype == "digest"
		for _, r := range splitParts(raw, p.BodyStart, p.BodyEnd, boundary) {
			if *budget <= 0 {
				break
			}
			*budget--
			p.Children = append(p.Children, parseEntity(raw, r.start, r.end, depth+1, budget, digest))
		}
	case p.Type == "message" && (p.Subtype == "rfc822" || p.Subtype == "global"):
		if p.BodyEnd > p.BodyStart {
			*budget--
			p.Nested = parseEntity(raw, p.BodyStart, p.BodyEnd, depth+1, budget, false)
		}
	}

	return p
}

// headerEnd returns the offset just past the blank line terminating the header
// block of raw[start:end], or start when the entity opens with a blank line.
func headerEnd(raw []byte, start, end int) int {
	i := start
	for i < end {
		nl := bytes.IndexByte(raw[i:end], '\n')
		lineEnd := end
		if nl >= 0 {
			lineEnd = i + nl + 1
		}
		line := raw[i:lineEnd]
		if len(bytes.Trim(line, "\r\n")) == 0 {
			return lineEnd
		}
		if nl < 0 {
			// Headers run to the end of the entity with no body.
			return end
		}
		i = lineEnd
	}
	return end
}

type span struct{ start, end int }

// splitParts locates the child entities delimited by boundary within
// raw[start:end], following RFC 2046 §5.1.1: a delimiter occupies a whole
// line, may carry trailing transport padding, and the CRLF preceding it
// belongs to the delimiter rather than to the part before it.
func splitParts(raw []byte, start, end int, boundary string) []span {
	delim := []byte("--" + boundary)

	type mark struct {
		lineStart int
		lineEnd   int
		closing   bool
	}
	var marks []mark

	i := start
	for i < end {
		nl := bytes.IndexByte(raw[i:end], '\n')
		lineEnd := end
		if nl >= 0 {
			lineEnd = i + nl + 1
		}
		line := bytes.TrimRight(raw[i:lineEnd], "\r\n")

		if bytes.HasPrefix(line, delim) {
			rest := line[len(delim):]
			closing := false
			if bytes.HasPrefix(rest, []byte("--")) {
				closing = true
				rest = rest[2:]
			}
			// Only transport padding may follow. This is what keeps a boundary
			// that is a prefix of a longer one from matching.
			if len(bytes.TrimLeft(rest, " \t")) == 0 {
				marks = append(marks, mark{lineStart: i, lineEnd: lineEnd, closing: closing})
				if closing {
					break
				}
			}
		}

		if nl < 0 {
			break
		}
		i = lineEnd
	}

	var out []span
	for k := 0; k+1 < len(marks); k++ {
		if marks[k].closing {
			break
		}
		ps, pe := marks[k].lineEnd, marks[k+1].lineStart
		if pe > ps && raw[pe-1] == '\n' {
			pe--
		}
		if pe > ps && raw[pe-1] == '\r' {
			pe--
		}
		if pe < ps {
			pe = ps
		}
		out = append(out, span{ps, pe})
	}

	// An unterminated multipart (no closing delimiter) still has a final part
	// running to the end of the enclosing entity.
	if n := len(marks); n > 0 && !marks[n-1].closing {
		out = append(out, span{marks[n-1].lineEnd, end})
	}

	return out
}

// Walk calls fn for every part in depth-first order, descending into embedded
// messages. Returning false from fn stops the traversal.
func (p *Part) Walk(fn func(*Part) bool) bool {
	if p == nil {
		return true
	}
	if !fn(p) {
		return false
	}
	for _, c := range p.Children {
		if !c.Walk(fn) {
			return false
		}
	}
	if p.Nested != nil {
		return p.Nested.Walk(fn)
	}
	return true
}

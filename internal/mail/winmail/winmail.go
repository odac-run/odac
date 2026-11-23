// Package winmail replaces the winmail.dat part Outlook and Exchange attach to
// outgoing mail with the files and body inside it, as standard MIME parts.
// Outside Outlook that part is one opaque file, so without this the
// recipient never sees the attachments it carries.
//
// The rewrite is a splice: every byte outside the converted parts stays where
// the sender put it, so the headers, the screening stamp and any other part
// reach the mailbox unchanged.
package winmail

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"odac/internal/mail/mimetree"
	"odac/internal/mail/tnef"
)

// Header marks a message this package rewrote, so a reader of the stored copy
// can tell it is not the one the sender transmitted.
const Header = "X-Odac-Tnef"

// maxRTFBytes bounds decompressed RTF across every part of one message. Each
// tnef.Decode has a budget of its own, and a message may carry many parts.
const maxRTFBytes = 32 << 20

// EnabledFromEnv reports whether ODAC_MAIL_TNEF leaves the conversion on. It
// is on unless the variable says off, the operator's way out if a converted
// message ever reads worse than the winmail.dat it replaced.
func EnabledFromEnv() bool {
	switch raw := strings.ToLower(strings.TrimSpace(os.Getenv("ODAC_MAIL_TNEF"))); raw {
	case "", "on":
		return true
	case "off":
		return false
	default:
		log.Printf("[SMTP Tnef] Ignoring invalid ODAC_MAIL_TNEF=%q, using on", raw)
		return true
	}
}

// Expand returns raw with every winmail.dat replaced by its content. It
// returns nil with no error when there is nothing to convert, and nil with an
// error when a part could not be converted completely. In both cases raw is
// the message to store, so a stream this package cannot read is delivered as
// it arrived rather than with part of it missing.
func Expand(raw []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("winmail: %v", r)
		}
	}()
	return expand(raw)
}

// expand is Expand without the recover, so the fuzz target sees a panic.
func expand(raw []byte) ([]byte, error) {
	root := mimetree.Parse(raw)
	var candidates []*mimetree.Part
	collect(root, &candidates)
	if len(candidates) == 0 {
		return nil, nil
	}

	nl := newline(raw)
	keepBody := hasBody(root, candidates)
	budget := maxRTFBytes
	left := map[*mimetree.Part]int{}
	var edits []edit
	for _, p := range candidates {
		msg, err := tnef.Decode(p.DecodedBody(raw), tnef.Limits{MaxRTFBytes: max(budget, 1)})
		if errors.Is(err, tnef.ErrNotTNEF) && !declared(p) {
			// A file that is only named winmail.dat.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("part at %d: %w", p.HeaderStart, err)
		}
		budget -= rtfBytes(msg)
		if !isMail(msg.Class) {
			// A meeting request or a receipt carries meaning its MIME
			// rendering would drop; Outlook recipients still read it.
			continue
		}
		b := &builder{nl: nl}
		e, err := replace(raw, root, p, b.content(msg, !keepBody), nl, left)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	if len(edits) == 0 {
		return nil, nil
	}
	return splice(raw, edits, Header+": converted"+nl), nil
}

// collect gathers the parts that may be TNEF. It does not descend into an
// attached message: that is a copy of someone else's mail, kept as sent.
func collect(p *mimetree.Part, out *[]*mimetree.Part) {
	if p.Type == "multipart" {
		for _, c := range p.Children {
			collect(c, out)
		}
		return
	}
	if declared(p) || (strings.EqualFold(p.Filename(), "winmail.dat") && p.Nested == nil) {
		*out = append(*out, p)
	}
}

func declared(p *mimetree.Part) bool {
	mt := p.MediaType()
	return mt == "application/ms-tnef" || mt == "application/vnd.ms-tnef"
}

// hasBody reports whether the message has a readable body outside the TNEF
// parts. Outlook sends a plain text copy beside winmail.dat, and a second
// body from inside it would show the same text twice.
func hasBody(p *mimetree.Part, tnefParts []*mimetree.Part) bool {
	if p.Type == "multipart" {
		for _, c := range p.Children {
			if hasBody(c, tnefParts) {
				return true
			}
		}
		return false
	}
	for _, t := range tnefParts {
		if p == t {
			return false
		}
	}
	mt := p.MediaType()
	return (mt == "text/plain" || mt == "text/html") && !p.IsAttachment()
}

// isMail reports whether a message class is ordinary mail. A stream that
// names no class is one, since that is what Outlook writes for it.
func isMail(class string) bool {
	c := strings.ToLower(strings.TrimSpace(class))
	return c == "" || c == "ipm.note" || strings.HasPrefix(c, "ipm.note.") || c == "ipm.microsoft mail.note"
}

func rtfBytes(m *tnef.Message) int {
	n := len(m.RTF)
	for _, a := range m.Attachments {
		if a.Message != nil {
			n += rtfBytes(a.Message)
		}
	}
	return n
}

// newline is the line ending the message uses. SMTP delivers CRLF, but a
// message that arrived with bare LF gets bare LF in what is spliced into it.
func newline(raw []byte) string {
	if i := bytes.IndexByte(raw, '\n'); i > 0 && raw[i-1] == '\r' {
		return "\r\n"
	}
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		return "\n"
	}
	return "\r\n"
}

// edit replaces raw[start:end].
type edit struct {
	start, end int
	data       []byte
}

// replace builds the edit that swaps p for e. A part inside a multipart is
// replaced whole, or removed with its delimiter when e is empty; the message
// itself keeps every header but the ones describing its content.
//
// left counts, per multipart, the parts not removed yet.
func replace(raw []byte, root, p *mimetree.Part, e *entity, nl string, left map[*mimetree.Part]int) (edit, error) {
	if p == root {
		return edit{0, p.BodyEnd, rootEntity(raw, p, e, nl)}, nil
	}
	parent := parentOf(root, p)
	if parent == nil {
		return edit{}, errors.New("winmail: part has no parent")
	}
	if e != nil {
		data := e.bytes()
		if e.subtype == "mixed" && parent.Subtype == "mixed" && parent.Params["boundary"] != "" {
			// Files beside the body read better as siblings than as a
			// multipart of their own.
			var b bytes.Buffer
			for i, c := range e.children {
				if i > 0 {
					b.WriteString("--" + parent.Params["boundary"] + nl)
				}
				c.write(&b)
			}
			data = b.Bytes()
		}
		// The line break that ends the entity is the one before the
		// delimiter, which is already there.
		return edit{p.HeaderStart, p.BodyEnd, bytes.TrimSuffix(data, []byte(nl))}, nil
	}
	if left[parent] == 0 {
		left[parent] = len(parent.Children)
	}
	left[parent]--
	if left[parent] == 0 {
		// A multipart needs a part, so the last one stays, empty: an empty
		// part shows nothing.
		return edit{p.HeaderStart, p.BodyEnd, nil}, nil
	}
	// The delimiter line ends at HeaderStart, and the line break before it
	// belongs to the delimiter (RFC 2046 5.1.1), so both go with the part.
	line := bytes.LastIndexByte(raw[:max(p.HeaderStart-1, 0)], '\n') + 1
	if line < parent.BodyStart {
		return edit{}, errors.New("winmail: delimiter not found")
	}
	if line == parent.BodyStart {
		// The first delimiter with no preamble has no line break before it.
		// The part goes with its own delimiter line, and the next delimiter
		// keeps its line, now first, without the break before it.
		end := p.BodyEnd
		if bytes.HasPrefix(raw[end:], []byte("\r\n")) {
			end += 2
		} else if end < len(raw) && raw[end] == '\n' {
			end++
		}
		return edit{line, end, nil}, nil
	}
	start := line - 1
	if start > 0 && raw[start-1] == '\r' {
		start--
	}
	return edit{start, p.BodyEnd, nil}, nil
}

func parentOf(p, child *mimetree.Part) *mimetree.Part {
	for _, c := range p.Children {
		if c == child {
			return p
		}
		if found := parentOf(c, child); found != nil {
			return found
		}
	}
	return nil
}

// contentFields are the header fields describing a message's content, which
// the converted content replaces at the top level.
var contentFields = map[string]bool{
	"content-type":              true,
	"content-transfer-encoding": true,
	"content-disposition":       true,
	"content-description":       true,
	"content-id":                true,
	"mime-version":              true,
}

// rootEntity rewrites a message that is a winmail.dat as a whole: Exchange
// sends mail that way between its own servers.
func rootEntity(raw []byte, p *mimetree.Part, e *entity, nl string) []byte {
	if e == nil {
		e = &entity{nl: nl, header: []string{"Content-Type: text/plain; charset=utf-8"}}
	}
	var b bytes.Buffer
	b.Grow(e.size() + p.BodyStart)
	drop := false
	for _, line := range bytes.SplitAfter(raw[:p.BodyStart], []byte("\n")) {
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			name, _, _ := bytes.Cut(line, []byte(":"))
			drop = contentFields[strings.ToLower(string(bytes.TrimSpace(name)))]
		}
		if !drop {
			b.Write(line)
		}
	}
	b.WriteString("MIME-Version: 1.0" + nl)
	e.write(&b)
	return b.Bytes()
}

// splice applies edits and prepends header. Edits replacing a part never
// overlap; two removals of neighbouring parts may share the line break
// between them, which goes once.
func splice(raw []byte, edits []edit, header string) []byte {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	size := len(raw) + len(header)
	for _, e := range edits {
		size += len(e.data) - (e.end - e.start)
	}
	out := make([]byte, 0, max(size, 0))
	out = append(out, header...)
	pos := 0
	for _, e := range edits {
		out = append(out, raw[pos:max(e.start, pos)]...)
		out = append(out, e.data...)
		pos = max(e.end, pos)
	}
	return append(out, raw[pos:]...)
}

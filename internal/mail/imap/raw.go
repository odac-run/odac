package imap

import (
	"bytes"
	"log"
	"strings"

	"odac/internal/mail/mimetree"
	"odac/internal/mail/storage"
)

// rawView gives access to a message's verbatim bytes and its MIME tree.
//
// Everything is loaded lazily: a client fetching only FLAGS must not pull a
// 30MB message off disk, and RFC822.SIZE is answered from the file size alone.
// Rows stored before raw messages were kept report unavailable, and the caller
// falls back to synthesizing a body from the parsed html/text columns.
type rawView struct {
	conn *Connection
	ref  string

	checked   bool
	present   bool
	size      int64
	bytesRead bool
	raw       []byte
	tree      *mimetree.Part
}

func newRawView(c *Connection, msg *storage.MessageRow) *rawView {
	ref := ""
	if msg.RawRef.Valid {
		ref = strings.TrimSpace(msg.RawRef.String)
	}
	return &rawView{conn: c, ref: ref}
}

// available reports whether the verbatim message can be served.
func (v *rawView) available() bool {
	if v.checked {
		return v.present
	}
	v.checked = true
	if v.ref == "" || v.conn == nil || v.conn.blobs == nil {
		return false
	}
	size, err := v.conn.blobs.Size(v.ref)
	if err != nil {
		// A row referencing a blob that is gone is recoverable: the parsed
		// columns still render a readable message.
		log.Printf("[IMAP] Raw message %s unavailable, using stored fields: %v", v.ref, err)
		return false
	}
	v.size = size
	v.present = true
	return true
}

// octets is the message size in bytes, read from the filesystem.
func (v *rawView) octets() int64 {
	if !v.available() {
		return 0
	}
	return v.size
}

// bytesAll returns the whole message, loading it on first use.
func (v *rawView) bytesAll() []byte {
	if !v.available() {
		return nil
	}
	if v.bytesRead {
		return v.raw
	}
	v.bytesRead = true
	data, err := v.conn.blobs.Get(v.ref)
	if err != nil {
		log.Printf("[IMAP] Raw message %s read failed: %v", v.ref, err)
		v.present = false
		return nil
	}
	v.raw = data
	return v.raw
}

// parts returns the parsed MIME tree of the message.
func (v *rawView) parts() *mimetree.Part {
	if v.tree != nil {
		return v.tree
	}
	raw := v.bytesAll()
	if raw == nil {
		return nil
	}
	v.tree = mimetree.Parse(raw)
	return v.tree
}

// section returns the bytes for an IMAP section specifier, reporting false
// when the specifier names a part the message does not have.
func (v *rawView) section(spec string) ([]byte, bool) {
	tree := v.parts()
	if tree == nil {
		return nil, false
	}
	part, kind, ok := tree.Resolve(spec)
	if !ok {
		return nil, false
	}
	start, end := part.Range(kind)
	raw := v.bytesAll()
	if start < 0 || end > len(raw) || end < start {
		return nil, false
	}

	// HEADER.FIELDS and HEADER.FIELDS.NOT narrow the header block that
	// KindHeader resolved to.
	upper := strings.ToUpper(spec)
	if idx := strings.Index(upper, "HEADER.FIELDS"); idx >= 0 {
		exclude := strings.Contains(upper, "HEADER.FIELDS.NOT")
		fields := parseFieldList(spec)
		return filterRawHeaders(raw[start:end], fields, exclude), true
	}
	return raw[start:end], true
}

// parseFieldList extracts the header names from a "HEADER.FIELDS (A B C)"
// specifier.
func parseFieldList(spec string) []string {
	open := strings.Index(spec, "(")
	closeIdx := strings.LastIndex(spec, ")")
	if open < 0 || closeIdx <= open {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(spec[open+1 : closeIdx]) {
		f = strings.Trim(f, `"`)
		if f != "" {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// filterRawHeaders keeps (or drops, when exclude is set) the named headers
// from a raw header block, preserving the original bytes of every line it
// keeps along with RFC 5322 folded continuations.
func filterRawHeaders(block []byte, fields []string, exclude bool) []byte {
	want := make(map[string]bool, len(fields))
	for _, f := range fields {
		want[f] = true
	}

	var out bytes.Buffer
	keeping := false
	i := 0
	for i < len(block) {
		nl := bytes.IndexByte(block[i:], '\n')
		lineEnd := len(block)
		if nl >= 0 {
			lineEnd = i + nl + 1
		}
		line := block[i:lineEnd]

		if len(bytes.Trim(line, "\r\n")) == 0 {
			break
		}

		if line[0] == ' ' || line[0] == '\t' {
			if keeping {
				out.Write(line)
			}
		} else {
			name := ""
			if colon := bytes.IndexByte(line, ':'); colon > 0 {
				name = strings.ToLower(strings.TrimSpace(string(line[:colon])))
			}
			keeping = want[name] != exclude
			if keeping {
				out.Write(line)
			}
		}

		if nl < 0 {
			break
		}
		i = lineEnd
	}

	// RFC 3501: a header-fields section is terminated by a blank line.
	out.WriteString("\r\n")
	return out.Bytes()
}

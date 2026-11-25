package mimetree

import (
	"bytes"
	"strings"
)

// Field is one header field as a lenient reader sees it.
type Field struct {
	Name  string // lowercased, surrounding space removed
	Value string // unfolded, surrounding space removed
	Line  string // the field as written, folded lines joined by a space
}

// HeaderEnd returns the offset just past the blank line that ends the
// message's header block, or len(raw) when it has none. It is the same
// boundary Parse gives the root part.
func HeaderEnd(raw []byte) int { return headerEnd(raw, 0, len(raw)) }

// Fields reads the fields of a header block the way mail clients do: a line
// without a colon is skipped instead of ending the block, as
// net/textproto does. The display columns and the spam filter both read From
// through it, so a field the filter never saw cannot be the one shown.
func Fields(block []byte) []Field {
	var out []Field
	for _, l := range bytes.Split(block, []byte("\n")) {
		line := strings.TrimRight(string(l), "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			last := &out[len(out)-1]
			cont := strings.TrimSpace(line)
			last.Line += " " + cont
			last.Value = strings.TrimSpace(last.Value + " " + cont)
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out = append(out, Field{
			Name:  strings.ToLower(strings.TrimSpace(name)),
			Value: strings.TrimSpace(value),
			Line:  line,
		})
	}
	return out
}

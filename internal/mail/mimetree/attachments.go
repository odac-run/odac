package mimetree

import "strconv"

// Attachment pairs an attachment part with the IMAP section path that
// addresses it. The path is exactly what Resolve accepts, so a stored partId
// can be handed straight back as a BODY[...] specifier.
type Attachment struct {
	PartID string
	Part   *Part
}

// Attachments lists every attachment in the message, including files carried
// inside a forwarded message. The forwarded message itself is listed too,
// since that is what a mail client offers the user to save.
func (p *Part) Attachments() []Attachment {
	var out []Attachment
	collectAttachments(p, "", true, &out)
	return out
}

func collectAttachments(p *Part, path string, isRoot bool, out *[]Attachment) {
	if p == nil {
		return
	}

	switch {
	case p.Type == "multipart":
		for i, c := range p.Children {
			collectAttachments(c, childPath(path, i+1), false, out)
		}

	case p.Nested != nil:
		if !isRoot {
			*out = append(*out, Attachment{PartID: path, Part: p})
		}
		// Resolve descends through Nested, so the embedded message's parts
		// address under this part's own path rather than one level deeper.
		if p.Nested.Type == "multipart" {
			for i, c := range p.Nested.Children {
				collectAttachments(c, childPath(path, i+1), false, out)
			}
		} else {
			collectAttachments(p.Nested, childPath(path, 1), false, out)
		}

	default:
		if !p.IsAttachment() {
			return
		}
		if isRoot {
			// A message that is itself a single attachment addresses as part 1.
			path = "1"
		}
		*out = append(*out, Attachment{PartID: path, Part: p})
	}
}

func childPath(prefix string, n int) string {
	if prefix == "" {
		return strconv.Itoa(n)
	}
	return prefix + "." + strconv.Itoa(n)
}

package message

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"

	"odac/internal/mail/mimetree"
	"odac/internal/mail/storage"
)

// Parsed holds the extracted fields from an RFC 2822 message,
// structured to match the existing SQLite schema from the Node.js implementation.
type Parsed struct {
	AttachmentsJSON string // JSON array of attachment metadata, empty when none
	From            string // JSON: {"value":[{"address":"...","name":"..."}]}
	HTML            string
	HeaderLinesJSON string // JSON array of {key, line} objects
	HeadersJSON     string // JSON object of header key→value
	MessageID       string
	Subject         string
	Text            string
	To              string // JSON: {"value":[{"address":"...","name":"..."}]}
}

// Parse splits an RFC 2822 message into headers and body,
// extracting structured fields compatible with the Node.js mailparser output format.
func Parse(raw []byte) Parsed {
	msg := Parsed{}
	content := string(raw)

	// Split headers from body at the first empty line
	headerEnd := strings.Index(content, "\r\n\r\n")
	if headerEnd < 0 {
		headerEnd = strings.Index(content, "\n\n")
	}

	var headerSection, bodySection string
	if headerEnd >= 0 {
		headerSection = content[:headerEnd]
		bodySection = content[headerEnd:]
		// Trim leading \r\n or \n\n separator
		bodySection = strings.TrimLeft(bodySection, "\r\n")
	} else {
		headerSection = content
	}

	// Parse headers — unfold continuation lines (RFC 2822 §2.2.3)
	var headerLines []map[string]string
	headers := make(map[string]string)

	lines := strings.Split(headerSection, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "" {
			continue
		}

		// Continuation line (starts with whitespace)
		if (line[0] == ' ' || line[0] == '\t') && len(headerLines) > 0 {
			last := headerLines[len(headerLines)-1]
			last["line"] += " " + strings.TrimSpace(line)
			key := last["key"]
			headers[key] += " " + strings.TrimSpace(line)
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		keyLower := strings.ToLower(key)

		headerLines = append(headerLines, map[string]string{
			"key":  keyLower,
			"line": line,
		})
		headers[keyLower] = value

		switch keyLower {
		case "subject":
			msg.Subject = value
		case "message-id":
			msg.MessageID = value
		case "from":
			msg.From = formatAddressJSON(value)
		case "to":
			msg.To = formatAddressJSON(value)
		}
	}

	headerLinesBytes, _ := json.Marshal(headerLines)
	msg.HeaderLinesJSON = string(headerLinesBytes)

	headersBytes, _ := json.Marshal(headers)
	msg.HeadersJSON = string(headersBytes)

	// One parse feeds both the display bodies and the attachment index; the
	// tree walk is over the header block's actual structure rather than a
	// guess made from the Content-Type header alone.
	tree := mimetree.Parse(raw)
	pickBodies(raw, tree, &msg.HTML, &msg.Text)
	msg.AttachmentsJSON = buildAttachmentsJSON(raw, tree)

	// If from/to weren't in headers, build from envelope
	if msg.From == "" {
		msg.From = `{"value":[]}`
	}
	if msg.To == "" {
		msg.To = `{"value":[]}`
	}

	return msg
}

// formatAddressJSON converts a raw email address header value into the JSON format
// used by Node.js mailparser: {"value":[{"address":"user@example.com","name":"User Name"}]}
func formatAddressJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return `{"value":[]}`
	}

	var address, name string

	// Parse "Display Name <email@example.com>" format
	if ltIdx := strings.LastIndex(raw, "<"); ltIdx >= 0 {
		gtIdx := strings.Index(raw[ltIdx:], ">")
		if gtIdx >= 0 {
			address = raw[ltIdx+1 : ltIdx+gtIdx]
			name = strings.TrimSpace(raw[:ltIdx])
			name = strings.Trim(name, `"'`)
		}
	}

	if address == "" {
		// Plain email address
		address = strings.Trim(raw, "<> ")
	}

	entry := map[string]string{"address": address}
	if name != "" {
		entry["name"] = name
	}

	result := map[string]any{"value": []map[string]string{entry}}
	b, _ := json.Marshal(result)
	return string(b)
}

// pickBodies fills in the display bodies of a message: the first
// non-attachment text/html and text/plain parts, decoded.
//
// Attachments are skipped explicitly. A text/plain file sent as an attachment
// used to overwrite the message body, because the old walk matched on
// Content-Type alone and never consulted Content-Disposition.
func pickBodies(raw []byte, p *mimetree.Part, html, text *string) {
	if p == nil {
		return
	}
	// An embedded message is an attachment in its own right; its body is not
	// the body of the message carrying it.
	if p.IsMessage() {
		return
	}
	if p.Type == "multipart" {
		for _, c := range p.Children {
			pickBodies(raw, c, html, text)
		}
		return
	}
	if p.IsAttachment() {
		return
	}
	switch p.MediaType() {
	case "text/html":
		if *html == "" {
			*html = string(p.DecodedBody(raw))
		}
	case "text/plain":
		if *text == "" {
			*text = string(p.DecodedBody(raw))
		}
	}
}

// attachmentMeta is one entry of the mail_received.attachments index.
//
// The attachment bytes are deliberately absent: they already live in the raw
// message, addressed by Offset and Length, so storing them again in SQL would
// duplicate the largest data in the system. The predecessor format inlined
// them as a JSON array of byte values, which inflated binary content roughly
// 3.6x and was loaded on every query that touched the row.
type attachmentMeta struct {
	Type               string            `json:"type"`
	ContentType        string            `json:"contentType"`
	PartID             string            `json:"partId"`
	ContentDisposition string            `json:"contentDisposition"`
	Filename           string            `json:"filename,omitempty"`
	ContentID          string            `json:"contentId,omitempty"`
	CID                string            `json:"cid,omitempty"`
	Headers            map[string]string `json:"headers"`
	Checksum           string            `json:"checksum"`
	Size               int               `json:"size"`
	Encoding           string            `json:"encoding"`
	Offset             int               `json:"offset"`
	Length             int               `json:"length"`
}

// buildAttachmentsJSON indexes the message's attachments. Offset and Length
// point at the still-encoded part body inside the raw message, which is what
// an IMAP BODY[partId] fetch returns verbatim; Size and Checksum describe the
// decoded file, matching what a client saves to disk.
func buildAttachmentsJSON(raw []byte, tree *mimetree.Part) string {
	found := tree.Attachments()
	if len(found) == 0 {
		return ""
	}

	metas := make([]attachmentMeta, 0, len(found))
	for _, a := range found {
		decoded := a.Part.DecodedBody(raw)
		// MD5 matches the checksum the previous Node.js implementation wrote,
		// and is a content fingerprint here, not a security primitive.
		sum := md5.Sum(decoded)

		disposition := a.Part.Disposition
		if disposition == "" {
			disposition = "attachment"
		}

		metas = append(metas, attachmentMeta{
			Type:               "attachment",
			ContentType:        a.Part.MediaType(),
			PartID:             a.PartID,
			ContentDisposition: disposition,
			Filename:           a.Part.Filename(),
			ContentID:          a.Part.ContentID,
			CID:                strings.Trim(a.Part.ContentID, "<>"),
			Headers:            map[string]string{},
			Checksum:           hex.EncodeToString(sum[:]),
			Size:               len(decoded),
			Encoding:           a.Part.Encoding,
			Offset:             a.Part.BodyStart,
			Length:             a.Part.Size(),
		})
	}

	out, err := json.Marshal(metas)
	if err != nil {
		return ""
	}
	return string(out)
}

// Apply fills the derived display columns of a stored message from the parse
// result, leaving identity and placement (Email, Mailbox, Flags, RawRef) to the
// caller. Delivery over SMTP and APPEND over IMAP both land in the same table
// and must agree on how a raw message maps onto it, so the mapping lives here
// rather than being spelled out again at each call site.
//
// These columns are derived display data. The verbatim message in the blob
// store remains the record, and rebuilding one from these fields is lossy.
func (p Parsed) Apply(row *storage.MessageRow) {
	row.Attachments = toNullString(p.AttachmentsJSON)
	row.From = toNullString(p.From)
	row.HTML = toNullString(p.HTML)
	row.HeaderLines = toNullString(p.HeaderLinesJSON)
	row.Headers = toNullString(p.HeadersJSON)
	row.MessageID = toNullString(p.MessageID)
	row.Subject = toNullString(p.Subject)
	row.Text = toNullString(p.Text)
	row.To = toNullString(p.To)
}

// toNullString maps an empty string to SQL NULL so an absent header is stored
// as NULL rather than as an empty value.
func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

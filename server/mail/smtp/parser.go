package smtp

import (
	"encoding/json"
	"strings"
)

// parsedMessage holds the extracted fields from an RFC 2822 message,
// structured to match the existing SQLite schema from the Node.js implementation.
type parsedMessage struct {
	from            string // JSON: {"value":[{"address":"...","name":"..."}]}
	headerLinesJSON string // JSON array of {key, line} objects
	headersJSON     string // JSON object of header key→value
	html            string
	messageID       string
	subject         string
	text            string
	to              string // JSON: {"value":[{"address":"...","name":"..."}]}
}

// parseMessage splits an RFC 2822 message into headers and body,
// extracting structured fields compatible with the Node.js mailparser output format.
func parseMessage(raw []byte) parsedMessage {
	msg := parsedMessage{}
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
			msg.subject = value
		case "message-id":
			msg.messageID = value
		case "from":
			msg.from = formatAddressJSON(value)
		case "to":
			msg.to = formatAddressJSON(value)
		}
	}

	headerLinesBytes, _ := json.Marshal(headerLines)
	msg.headerLinesJSON = string(headerLinesBytes)

	headersBytes, _ := json.Marshal(headers)
	msg.headersJSON = string(headersBytes)

	// Determine body content type from headers
	contentType := strings.ToLower(headers["content-type"])

	if strings.Contains(contentType, "text/html") {
		msg.html = bodySection
	} else if strings.Contains(contentType, "multipart/") {
		// Extract boundary and parse MIME parts
		msg.html, msg.text = parseMIMEBody(contentType, bodySection)
	} else {
		// Default to text/plain
		msg.text = bodySection
	}

	// If from/to weren't in headers, build from envelope
	if msg.from == "" {
		msg.from = `{"value":[]}`
	}
	if msg.to == "" {
		msg.to = `{"value":[]}`
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

// parseMIMEBody extracts text/plain and text/html parts from a multipart message.
func parseMIMEBody(contentType, body string) (html, text string) {
	// Extract boundary from Content-Type header
	boundary := ""
	for _, param := range strings.Split(contentType, ";") {
		param = strings.TrimSpace(param)
		if strings.HasPrefix(strings.ToLower(param), "boundary=") {
			boundary = strings.Trim(param[9:], `"' `)
			break
		}
	}

	if boundary == "" {
		return "", body
	}

	delimiter := "--" + boundary
	parts := strings.Split(body, delimiter)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "--" {
			continue
		}

		// Split part headers from part body
		partHeaderEnd := strings.Index(part, "\r\n\r\n")
		if partHeaderEnd < 0 {
			partHeaderEnd = strings.Index(part, "\n\n")
		}
		if partHeaderEnd < 0 {
			continue
		}

		partHeaders := strings.ToLower(part[:partHeaderEnd])
		partBody := strings.TrimLeft(part[partHeaderEnd:], "\r\n")

		// Recurse into nested multipart
		if strings.Contains(partHeaders, "multipart/") {
			for _, line := range strings.Split(part[:partHeaderEnd], "\n") {
				line = strings.TrimSpace(strings.ToLower(line))
				if strings.HasPrefix(line, "content-type:") {
					subHTML, subText := parseMIMEBody(line[13:], partBody)
					if subHTML != "" {
						html = subHTML
					}
					if subText != "" {
						text = subText
					}
					break
				}
			}
			continue
		}

		if strings.Contains(partHeaders, "text/html") {
			html = partBody
		} else if strings.Contains(partHeaders, "text/plain") {
			text = partBody
		}
	}

	return html, text
}

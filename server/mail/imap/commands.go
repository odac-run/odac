package imap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"odac-mail/auth"
	"odac-mail/storage"
)

func (c *Connection) cmdCapability(tag string) {
	c.write(fmt.Sprintf("* CAPABILITY %s\r\n", capabilities))
	c.write(fmt.Sprintf("%s OK CAPABILITY completed\r\n", tag))
}

func (c *Connection) cmdLogout(tag string) {
	c.write("* BYE IMAP4rev1 Server logging out\r\n")
	c.write(fmt.Sprintf("%s OK LOGOUT completed\r\n", tag))
}

func (c *Connection) cmdLogin(tag, args string) {
	parts := splitArgs(args)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid arguments\r\n", tag))
		return
	}

	username := unquote(parts[0])
	password := unquote(parts[1])
	ip := extractConnIP(c.conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	account, err := c.store.AccountExists(ctx, username)
	if err != nil || account == nil {
		c.firewall.HandleFailedAuth(ip)
		c.write(fmt.Sprintf("%s NO Authentication failed\r\n", tag))
		return
	}

	match, err := authComparePassword(password, account.Password)
	if err != nil || !match {
		c.firewall.HandleFailedAuth(ip)
		c.write(fmt.Sprintf("%s NO Authentication failed\r\n", tag))
		return
	}

	c.firewall.ClearAttempts(ip)
	c.auth = username
	log.Printf("[IMAP] User authenticated: %s from %s", username, ip)

	// Transparent password upgrade: rehash legacy N=16384 → current N=32768
	if auth.NeedsRehash(account.Password) {
		go func() {
			newHash, err := auth.HashPassword(password)
			if err != nil {
				return
			}
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			if err := c.store.AccountUpdatePassword(ctx2, username, newHash); err == nil {
				log.Printf("[IMAP] Password rehashed for %s (scrypt N upgraded)", username)
			}
		}()
	}

	c.write(fmt.Sprintf("%s OK Authentication successful\r\n", tag))
}

func (c *Connection) cmdAuthenticate(tag, args string) {
	mech := strings.ToUpper(strings.TrimSpace(args))

	if mech != "PLAIN" && mech != "LOGIN" {
		c.write(fmt.Sprintf("%s NO Unsupported authentication mechanism\r\n", tag))
		return
	}

	// Send empty challenge
	c.write("+ \r\n")

	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := c.reader.ReadString('\n')
	if err != nil {
		c.write(fmt.Sprintf("%s NO Authentication timeout\r\n", tag))
		return
	}

	line = strings.TrimRight(line, "\r\n")
	if line == "*" {
		c.write(fmt.Sprintf("%s BAD Authentication cancelled\r\n", tag))
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		c.write(fmt.Sprintf("%s NO Authentication data invalid\r\n", tag))
		return
	}

	// PLAIN: \0username\0password
	parts := strings.SplitN(string(decoded), "\x00", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		c.write(fmt.Sprintf("%s NO Authentication failed\r\n", tag))
		return
	}

	// Reuse login logic
	c.cmdLogin(tag, fmt.Sprintf(`"%s" "%s"`, parts[1], parts[2]))
}

func (c *Connection) cmdSelect(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	box := unquote(strings.TrimSpace(args))
	if box == "" {
		c.write(fmt.Sprintf("%s NO Mailbox name required\r\n", tag))
		return
	}

	c.mailbox = box

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := c.store.MailboxSelect(ctx, c.auth, c.mailbox)
	if err != nil {
		c.write(fmt.Sprintf("%s NO SELECT failed\r\n", tag))
		return
	}

	flagsList := strings.Join(permanentFlags, " ")
	c.write(fmt.Sprintf("* FLAGS (%s)\r\n", flagsList))
	c.write(fmt.Sprintf("* OK [PERMANENTFLAGS (%s)] Flags permitted\r\n", flagsList))
	c.write(fmt.Sprintf("* %d EXISTS\r\n", stats.Exists))
	c.write(fmt.Sprintf("* %d RECENT\r\n", stats.Unseen))
	c.write(fmt.Sprintf("* OK [UNSEEN %d] Message %d is first unseen\r\n", stats.Unseen, stats.Unseen))
	c.write(fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid\r\n", stats.UIDValidity))
	c.write(fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID\r\n", stats.UIDNext))
	c.write(fmt.Sprintf("%s OK [READ-WRITE] SELECT completed\r\n", tag))
}

func (c *Connection) cmdExamine(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	box := unquote(strings.TrimSpace(args))
	if box == "" {
		c.write(fmt.Sprintf("%s NO Mailbox name required\r\n", tag))
		return
	}

	c.mailbox = box

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := c.store.MailboxSelect(ctx, c.auth, c.mailbox)
	if err != nil {
		c.write(fmt.Sprintf("%s NO EXAMINE failed\r\n", tag))
		return
	}

	flagsList := strings.Join(permanentFlags, " ")
	c.write(fmt.Sprintf("* FLAGS (%s)\r\n", flagsList))
	c.write(fmt.Sprintf("* OK [PERMANENTFLAGS (%s)] Flags permitted\r\n", flagsList))
	c.write(fmt.Sprintf("* %d EXISTS\r\n", stats.Exists))
	c.write(fmt.Sprintf("* %d RECENT\r\n", stats.Unseen))
	c.write(fmt.Sprintf("* OK [UNSEEN %d] Message %d is first unseen\r\n", stats.Unseen, stats.Unseen))
	c.write(fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid\r\n", stats.UIDValidity))
	c.write(fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID\r\n", stats.UIDNext))
	c.write(fmt.Sprintf("%s OK [READ-ONLY] EXAMINE completed\r\n", tag))
}

func (c *Connection) cmdList(tag string) {
	if !c.requireAuth(tag) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	boxes, err := c.store.MailboxList(ctx, c.auth)
	if err != nil {
		c.write(fmt.Sprintf("%s NO LIST failed\r\n", tag))
		return
	}

	for _, box := range boxes {
		c.write(fmt.Sprintf("* LIST (\\HasNoChildren) \"/\" %s\r\n", box))
	}
	c.write(fmt.Sprintf("%s OK LIST completed\r\n", tag))
}

func (c *Connection) cmdLsub(tag string) {
	if !c.requireAuth(tag) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	boxes, err := c.store.MailboxList(ctx, c.auth)
	if err != nil {
		c.write(fmt.Sprintf("%s NO LSUB failed\r\n", tag))
		return
	}

	for _, box := range boxes {
		c.write(fmt.Sprintf("* LSUB (\\HasNoChildren) \"/\" \"%s\"\r\n", box))
	}
	c.write(fmt.Sprintf("%s OK LSUB completed\r\n", tag))
}

func (c *Connection) cmdStatus(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	parts := strings.SplitN(args, " ", 2)
	mailbox := unquote(parts[0])

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := c.store.MailboxSelect(ctx, c.auth, mailbox)
	if err != nil {
		c.write(fmt.Sprintf("%s NO STATUS failed\r\n", tag))
		return
	}

	// Parse requested fields
	fields := "MESSAGES RECENT UIDNEXT UIDVALIDITY UNSEEN"
	if len(parts) > 1 {
		fields = strings.ToUpper(parts[1])
	}

	var result []string
	if strings.Contains(fields, "MESSAGES") {
		result = append(result, fmt.Sprintf("MESSAGES %d", stats.Exists))
	}
	if strings.Contains(fields, "RECENT") {
		result = append(result, fmt.Sprintf("RECENT %d", stats.Unseen))
	}
	if strings.Contains(fields, "UIDNEXT") {
		result = append(result, fmt.Sprintf("UIDNEXT %d", stats.UIDNext))
	}
	if strings.Contains(fields, "UIDVALIDITY") {
		result = append(result, fmt.Sprintf("UIDVALIDITY %d", stats.UIDValidity))
	}
	if strings.Contains(fields, "UNSEEN") {
		result = append(result, fmt.Sprintf("UNSEEN %d", stats.Unseen))
	}

	c.write(fmt.Sprintf("* STATUS %s (%s)\r\n", mailbox, strings.Join(result, " ")))
	c.write(fmt.Sprintf("%s OK STATUS completed\r\n", tag))
}

func (c *Connection) cmdCreate(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	mailbox := unquote(strings.TrimSpace(args))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.store.MailboxCreate(ctx, c.auth, mailbox); err != nil {
		c.write(fmt.Sprintf("%s NO CREATE failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK CREATE completed\r\n", tag))
}

func (c *Connection) cmdDelete(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	mailbox := unquote(strings.TrimSpace(args))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.store.MailboxDelete(ctx, c.auth, mailbox); err != nil {
		c.write(fmt.Sprintf("%s NO DELETE failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK DELETE completed\r\n", tag))
}

func (c *Connection) cmdRename(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	parts := splitArgs(args)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid arguments\r\n", tag))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.store.MailboxRename(ctx, c.auth, unquote(parts[0]), unquote(parts[1])); err != nil {
		c.write(fmt.Sprintf("%s NO RENAME failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK RENAME completed\r\n", tag))
}

func (c *Connection) cmdFetch(tag, cmd, args string) {
	if !c.requireMailbox(tag) {
		return
	}

	// Handle "UID FETCH" prefix
	fetchArgs := args
	if cmd == "UID" {
		parts := strings.SplitN(args, " ", 2)
		if len(parts) < 2 || strings.ToUpper(parts[0]) != "FETCH" {
			c.write(fmt.Sprintf("%s BAD Unknown UID command\r\n", tag))
			return
		}
		fetchArgs = parts[1]
	}

	// Parse sequence set and data items
	parts := strings.SplitN(fetchArgs, " ", 2)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid FETCH arguments\r\n", tag))
		return
	}

	seqSet := parts[0]
	dataItems := strings.ToUpper(parts[1])

	// Parse UID range
	var uidMin, uidMax int64
	if seqSet != "ALL" && seqSet != "*" {
		if strings.Contains(seqSet, ":") {
			rangeParts := strings.SplitN(seqSet, ":", 2)
			uidMin, _ = strconv.ParseInt(rangeParts[0], 10, 64)
			if rangeParts[1] != "*" {
				uidMax, _ = strconv.ParseInt(rangeParts[1], 10, 64)
			}
		} else {
			uid, _ := strconv.ParseInt(seqSet, 10, 64)
			uidMin = uid
			uidMax = uid
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	messages, err := c.store.MessageFetch(ctx, c.auth, c.mailbox, uidMin, uidMax)
	if err != nil {
		c.write(fmt.Sprintf("%s NO FETCH failed\r\n", tag))
		return
	}

	for _, msg := range messages {
		c.write(fmt.Sprintf("* %d FETCH (", msg.UID))
		c.writeFetchItems(dataItems, &msg)
		c.write(")\r\n")
	}
	c.write(fmt.Sprintf("%s OK FETCH completed\r\n", tag))
}

func (c *Connection) writeFetchItems(items string, msg *storage.MessageRow) {
	if strings.Contains(items, "UID") {
		c.write(fmt.Sprintf("UID %d ", msg.UID))
	}
	if strings.Contains(items, "FLAGS") {
		flags := parseJSONFlags(msg.Flags.String)
		c.write(fmt.Sprintf("FLAGS (%s) ", strings.Join(flags, " ")))
	}
	if strings.Contains(items, "INTERNALDATE") {
		date := msg.Date.String
		if date != "" {
			c.write(fmt.Sprintf("INTERNALDATE \"%s\" ", date))
		}
	}
	if strings.Contains(items, "RFC822.SIZE") {
		size := len(msg.HTML.String) + len(msg.Text.String)
		c.write(fmt.Sprintf("RFC822.SIZE %d ", size))
	}
	if strings.Contains(items, "ENVELOPE") {
		c.writeEnvelope(msg)
	}
	if strings.Contains(items, "BODYSTRUCTURE") {
		c.writeBodyStructure(msg)
	}
	if strings.Contains(items, "BODY") && !strings.Contains(items, "BODYSTRUCTURE") {
		c.writeBody(items, msg)
	}
}

func (c *Connection) writeEnvelope(msg *storage.MessageRow) {
	date := msg.Date.String
	subject := msg.Subject.String
	from := ""
	if msg.From.Valid {
		from = msg.From.String
	}
	c.write(fmt.Sprintf("ENVELOPE (\"%s\" \"%s\" \"%s\") ", date, subject, from))
}

func (c *Connection) writeBodyStructure(msg *storage.MessageRow) {
	hasText := msg.Text.Valid && msg.Text.String != ""
	hasHTML := msg.HTML.Valid && msg.HTML.String != ""

	if hasText && hasHTML {
		textSize := len(msg.Text.String)
		htmlSize := len(msg.HTML.String)
		c.write(fmt.Sprintf("BODYSTRUCTURE ((\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"BASE64\" %d NIL NIL NIL NIL)(\"TEXT\" \"HTML\" (\"CHARSET\" \"UTF-8\") NIL NIL \"BASE64\" %d) \"ALTERNATIVE\" NIL NIL NIL) ", textSize, htmlSize))
	} else if hasHTML {
		c.write(fmt.Sprintf("BODYSTRUCTURE (\"TEXT\" \"HTML\" (\"CHARSET\" \"UTF-8\") NIL NIL \"BASE64\" %d) ", len(msg.HTML.String)))
	} else {
		size := 0
		if hasText {
			size = len(msg.Text.String)
		}
		c.write(fmt.Sprintf("BODYSTRUCTURE (\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"BASE64\" %d) ", size))
	}
}

func (c *Connection) writeBody(items string, msg *storage.MessageRow) {
	// Determine what body section is requested
	var content string
	if strings.Contains(items, "BODY[HEADER") {
		// Build headers from headerLines
		content = buildHeaders(msg)
	} else if strings.Contains(items, "BODY[TEXT]") || strings.Contains(items, "BODY[]") {
		content = buildFullBody(msg)
	} else {
		content = buildFullBody(msg)
	}

	c.write(fmt.Sprintf("BODY[] {%d}\r\n%s", len(content), content))
}

func (c *Connection) cmdStore(tag, args string) {
	if !c.requireMailbox(tag) {
		return
	}

	// Parse: <sequence set> <data item> <value>
	parts := strings.SplitN(args, " ", 3)
	if len(parts) < 3 {
		c.write(fmt.Sprintf("%s NO Invalid STORE arguments\r\n", tag))
		return
	}

	seqSet := parts[0]
	dataItem := strings.ToUpper(parts[1])
	flagStr := parts[2]

	// Parse action
	var action string
	switch {
	case strings.HasPrefix(dataItem, "+FLAGS"):
		action = "add"
	case strings.HasPrefix(dataItem, "-FLAGS"):
		action = "remove"
	case strings.HasPrefix(dataItem, "FLAGS"):
		action = "set"
	default:
		c.write(fmt.Sprintf("%s NO Unknown STORE data item\r\n", tag))
		return
	}

	// Parse flags
	flagStr = strings.Trim(flagStr, "()")
	var flags []string
	for _, f := range strings.Fields(flagStr) {
		flags = append(flags, strings.ToLower(strings.TrimPrefix(f, "\\")))
	}

	// Parse UID range
	var uidMin, uidMax int64
	if strings.Contains(seqSet, ":") {
		rangeParts := strings.SplitN(seqSet, ":", 2)
		uidMin, _ = strconv.ParseInt(rangeParts[0], 10, 64)
		uidMax, _ = strconv.ParseInt(rangeParts[1], 10, 64)
	} else {
		uid, _ := strconv.ParseInt(seqSet, 10, 64)
		uidMin = uid
		uidMax = uid
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.store.MessageStoreFlags(ctx, c.auth, uidMin, uidMax, action, flags); err != nil {
		c.write(fmt.Sprintf("%s NO STORE failed\r\n", tag))
		return
	}

	c.write(fmt.Sprintf("%s OK STORE completed\r\n", tag))
}

func (c *Connection) cmdExpunge(tag string) {
	if !c.requireMailbox(tag) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uids, err := c.store.MessageExpunge(ctx, c.auth, c.mailbox)
	if err != nil {
		c.write(fmt.Sprintf("%s NO EXPUNGE failed\r\n", tag))
		return
	}

	for _, uid := range uids {
		c.write(fmt.Sprintf("* %d EXPUNGE\r\n", uid))
	}
	c.write(fmt.Sprintf("%s OK EXPUNGE completed\r\n", tag))
}

func (c *Connection) cmdClose(tag string) {
	if !c.requireMailbox(tag) {
		return
	}

	// CLOSE implicitly expunges
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.store.MessageExpunge(ctx, c.auth, c.mailbox)

	c.mailbox = ""
	c.write(fmt.Sprintf("%s OK CLOSE completed\r\n", tag))
}

func (c *Connection) cmdSearch(tag, args string) {
	if !c.requireMailbox(tag) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Simplified search: fetch all UIDs for the mailbox
	messages, err := c.store.MessageFetch(ctx, c.auth, c.mailbox, 0, 0)
	if err != nil {
		c.write(fmt.Sprintf("%s NO SEARCH failed\r\n", tag))
		return
	}

	var uids []string
	criteria := strings.ToUpper(args)
	for _, msg := range messages {
		match := true

		if strings.Contains(criteria, "UNSEEN") {
			flags := msg.Flags.String
			if strings.Contains(flags, "seen") {
				match = false
			}
		}
		if strings.Contains(criteria, "SEEN") && !strings.Contains(criteria, "UNSEEN") {
			flags := msg.Flags.String
			if !strings.Contains(flags, "seen") {
				match = false
			}
		}
		if strings.Contains(criteria, "DELETED") {
			flags := msg.Flags.String
			if !strings.Contains(flags, "deleted") {
				match = false
			}
		}

		if match {
			uids = append(uids, strconv.FormatInt(msg.UID, 10))
		}
	}

	c.write(fmt.Sprintf("* SEARCH %s\r\n", strings.Join(uids, " ")))
	c.write(fmt.Sprintf("%s OK SEARCH completed\r\n", tag))
}

func (c *Connection) cmdIdle(tag string) {
	if !c.requireMailbox(tag) {
		return
	}

	c.write("+ idling\r\n")

	// Wait for DONE command with periodic checks
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Minute))
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return // Connection closed
		}

		if strings.ToUpper(strings.TrimSpace(line)) == "DONE" {
			c.write(fmt.Sprintf("%s OK IDLE terminated\r\n", tag))
			return
		}
	}
}

func (c *Connection) cmdCopy(tag, args string) {
	if !c.requireMailbox(tag) {
		return
	}

	parts := splitArgs(args)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid COPY arguments\r\n", tag))
		return
	}

	seqSet := parts[0]
	targetMailbox := unquote(parts[1])

	var uidMin, uidMax int64
	if strings.Contains(seqSet, ":") {
		rangeParts := strings.SplitN(seqSet, ":", 2)
		uidMin, _ = strconv.ParseInt(rangeParts[0], 10, 64)
		if rangeParts[1] != "*" {
			uidMax, _ = strconv.ParseInt(rangeParts[1], 10, 64)
		}
	} else {
		uid, _ := strconv.ParseInt(seqSet, 10, 64)
		uidMin = uid
		uidMax = uid
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.store.MessageCopy(ctx, c.auth, uidMin, uidMax, c.mailbox, targetMailbox); err != nil {
		c.write(fmt.Sprintf("%s NO COPY failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK COPY completed\r\n", tag))
}

func (c *Connection) cmdAppend(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	parts := splitArgs(args)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid APPEND arguments\r\n", tag))
		return
	}

	mailbox := unquote(parts[0])

	// Parse optional flags and literal size
	flags := "[]"
	var literalSize int64
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "(") {
			flags = "[" + strings.Trim(p, "()") + "]"
		}
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			literalSize, _ = strconv.ParseInt(p[1:len(p)-1], 10, 64)
		}
	}

	if literalSize <= 0 {
		literalSize = 10 * 1024 * 1024 // Default 10MB max
	}

	// Send continuation request
	c.write("+ Ready for literal data\r\n")

	// Read literal data
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	buf := make([]byte, literalSize)
	n, err := c.reader.Read(buf)
	if err != nil {
		c.write(fmt.Sprintf("%s NO APPEND failed\r\n", tag))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	msg := &storage.MessageRow{
		Email:   c.auth,
		Flags:   toNullString(flags),
		HTML:    toNullString(string(buf[:n])),
		Mailbox: mailbox,
	}

	if err := c.store.MessageStore(ctx, msg); err != nil {
		c.write(fmt.Sprintf("%s NO APPEND failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK APPEND completed\r\n", tag))
}

func (c *Connection) write(data string) {
	c.conn.SetWriteDeadline(time.Now().Add(commandTimeout))
	c.conn.Write([]byte(data))
}

// --- Helpers ---

func splitArgs(s string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false

	for _, ch := range s {
		switch {
		case ch == '"':
			inQuote = !inQuote
			current.WriteRune(ch)
		case ch == ' ' && !inQuote:
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func parseJSONFlags(flagsJSON string) []string {
	if flagsJSON == "" || flagsJSON == "[]" {
		return nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(flagsJSON), &raw); err != nil {
		return nil
	}
	var flags []string
	for _, f := range raw {
		flags = append(flags, "\\"+strings.Title(f))
	}
	return flags
}

func buildHeaders(msg *storage.MessageRow) string {
	if !msg.HeaderLines.Valid || msg.HeaderLines.String == "" {
		return ""
	}
	var lines []struct {
		Key  string `json:"key"`
		Line string `json:"line"`
	}
	if err := json.Unmarshal([]byte(msg.HeaderLines.String), &lines); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.Line)
		sb.WriteString("\r\n")
	}
	return sb.String()
}

func buildFullBody(msg *storage.MessageRow) string {
	var sb strings.Builder

	// Headers
	headers := buildHeaders(msg)
	if headers != "" {
		sb.WriteString(headers)
		sb.WriteString("\r\n")
	}

	// Body content
	if msg.HTML.Valid && msg.HTML.String != "" {
		sb.WriteString(msg.HTML.String)
	} else if msg.Text.Valid && msg.Text.String != "" {
		sb.WriteString(msg.Text.String)
	}

	return sb.String()
}

// authComparePassword wraps the auth package to avoid circular imports.
func authComparePassword(password, storedHash string) (bool, error) {
	return auth.ComparePassword(password, storedHash)
}

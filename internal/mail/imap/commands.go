package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"odac/internal/mail/auth"
	"odac/internal/mail/config"
	"odac/internal/mail/limits"
	"odac/internal/mail/message"
	"odac/internal/mail/storage"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func (c *Connection) cmdCapability(tag string) {
	c.write(fmt.Sprintf("* CAPABILITY %s\r\n", c.capabilityString()))
	c.write(fmt.Sprintf("%s OK CAPABILITY completed\r\n", tag))
}

// pushMailboxUpdates emits untagged EXISTS / RECENT responses when the
// selected mailbox state has changed since the last report. RFC 3501 §6.1.2
// requires NOOP and IDLE to deliver these so clients learn about new mail
// without re-issuing SELECT.
func (c *Connection) pushMailboxUpdates() {
	if c.mailbox == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats, err := c.store.MailboxSelect(ctx, c.auth, c.mailbox)
	if err != nil {
		return
	}
	if stats.Exists != c.lastExists {
		c.write(fmt.Sprintf("* %d EXISTS\r\n", stats.Exists))
		c.lastExists = stats.Exists
	}
	if stats.Unseen != c.lastUnseen {
		c.write(fmt.Sprintf("* %d RECENT\r\n", stats.Unseen))
		c.lastUnseen = stats.Unseen
	}
}

// cmdStartTLS upgrades the plaintext connection to TLS per RFC 2595 / RFC 3501 §6.2.1.
// Returns false if the connection should be closed (handshake or protocol error).
func (c *Connection) cmdStartTLS(tag string) bool {
	if c.tls {
		c.write(fmt.Sprintf("%s NO TLS already active\r\n", tag))
		return true
	}
	if c.tlsConfig == nil {
		c.write(fmt.Sprintf("%s NO STARTTLS not configured\r\n", tag))
		return true
	}

	// RFC 3501 §6.2.1: client MUST NOT pipeline commands after STARTTLS.
	// Buffered bytes between the OK and the TLS ClientHello indicate either a
	// broken client or a plaintext-injection attack — refuse the upgrade.
	if c.reader.Buffered() > 0 {
		c.write(fmt.Sprintf("%s BAD Unexpected pipelined data before TLS handshake\r\n", tag))
		return false
	}

	c.write(fmt.Sprintf("%s OK Begin TLS negotiation now\r\n", tag))

	c.conn.SetReadDeadline(time.Time{})
	tlsConn := tls.Server(c.conn, c.tlsConfig)
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		log.Printf("[IMAP] STARTTLS handshake failed from %s: %v", extractConnIP(c.conn), err)
		return false
	}

	// RFC 3501: discard all session state from the unprotected phase to defeat
	// MITM attacks that may have manipulated pre-TLS commands or capabilities.
	c.conn = tlsConn
	c.reader = bufio.NewReaderSize(tlsConn, maxLineSize)
	c.tls = true
	c.auth = ""
	c.mailbox = ""
	return true
}

func (c *Connection) cmdLogout(tag string) {
	c.write("* BYE IMAP4rev1 Server logging out\r\n")
	c.write(fmt.Sprintf("%s OK LOGOUT completed\r\n", tag))
}

func (c *Connection) cmdLogin(tag, args string) {
	if !c.tls {
		c.write(fmt.Sprintf("%s NO [PRIVACYREQUIRED] LOGIN requires TLS — use STARTTLS or connect on port 993\r\n", tag))
		return
	}

	parts := splitArgs(args)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid arguments\r\n", tag))
		return
	}

	c.authenticateUser(tag, unquote(parts[0]), unquote(parts[1]))
}

// authenticateUser runs the credential check shared by LOGIN and AUTHENTICATE.
// Both entry points hand over already-decoded values: re-serialising credentials
// into a command line would let a `"` inside a username shift the argument
// boundaries of the parser that re-reads it.
func (c *Connection) authenticateUser(tag, username, password string) {
	ip := extractConnIP(c.conn)

	// A username reaches the logs; control characters there forge log records.
	if username == "" || hasControlChars(username) {
		c.firewall.HandleFailedAuth(ip)
		c.write(fmt.Sprintf("%s NO Authentication failed\r\n", tag))
		return
	}

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

	if reason := c.limit.BindUser(username); reason != limits.ReasonOK {
		log.Printf("[IMAP] Post-auth limit hit for %s from %s: %s", username, ip, reason)
		c.write("* BYE [LIMIT] Too many connections for user\r\n")
		c.write(fmt.Sprintf("%s NO [LIMIT] Too many connections for user\r\n", tag))
		c.conn.Close()
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
	if !c.tls {
		c.write(fmt.Sprintf("%s NO [PRIVACYREQUIRED] AUTHENTICATE requires TLS — use STARTTLS or connect on port 993\r\n", tag))
		return
	}

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

	c.authenticateUser(tag, parts[1], parts[2])
}

func (c *Connection) cmdSelect(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	box := unquote(strings.TrimSpace(args))
	if !c.requireMailboxName(tag, box) {
		return
	}

	c.mailbox = box

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := c.store.MailboxSelect(ctx, c.auth, c.mailbox)
	if err != nil {
		log.Printf("[IMAP] SELECT %q for %q failed: %v", c.mailbox, c.auth, err)
		c.write(fmt.Sprintf("%s NO SELECT failed\r\n", tag))
		return
	}

	c.write(fmt.Sprintf("* FLAGS (%s)\r\n", strings.Join(flagsList, " ")))
	c.write(fmt.Sprintf("* OK [PERMANENTFLAGS (%s)] Flags permitted\r\n", strings.Join(permanentFlags, " ")))
	c.write(fmt.Sprintf("* %d EXISTS\r\n", stats.Exists))
	c.write(fmt.Sprintf("* %d RECENT\r\n", stats.Unseen))
	if stats.Unseen > 0 {
		c.write(fmt.Sprintf("* OK [UNSEEN %d] Message %d is first unseen\r\n", stats.Unseen, stats.Unseen))
	}
	c.write(fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid\r\n", stats.UIDValidity))
	c.write(fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID\r\n", stats.UIDNext))
	c.lastExists = stats.Exists
	c.lastUnseen = stats.Unseen
	c.write(fmt.Sprintf("%s OK [READ-WRITE] SELECT completed\r\n", tag))
}

func (c *Connection) cmdExamine(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	box := unquote(strings.TrimSpace(args))
	if !c.requireMailboxName(tag, box) {
		return
	}

	c.mailbox = box

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := c.store.MailboxSelect(ctx, c.auth, c.mailbox)
	if err != nil {
		log.Printf("[IMAP] EXAMINE %q for %q failed: %v", c.mailbox, c.auth, err)
		c.write(fmt.Sprintf("%s NO EXAMINE failed\r\n", tag))
		return
	}

	c.write(fmt.Sprintf("* FLAGS (%s)\r\n", strings.Join(flagsList, " ")))
	c.write(fmt.Sprintf("* OK [PERMANENTFLAGS (%s)] Flags permitted\r\n", strings.Join(permanentFlags, " ")))
	c.write(fmt.Sprintf("* %d EXISTS\r\n", stats.Exists))
	c.write(fmt.Sprintf("* %d RECENT\r\n", stats.Unseen))
	if stats.Unseen > 0 {
		c.write(fmt.Sprintf("* OK [UNSEEN %d] Message %d is first unseen\r\n", stats.Unseen, stats.Unseen))
	}
	c.write(fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid\r\n", stats.UIDValidity))
	c.write(fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID\r\n", stats.UIDNext))
	c.lastExists = stats.Exists
	c.lastUnseen = stats.Unseen
	c.write(fmt.Sprintf("%s OK [READ-ONLY] EXAMINE completed\r\n", tag))
}

func (c *Connection) cmdList(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	// RFC 5258 LIST-EXTENDED: optional selection-options in parentheses before reference.
	// Example: LIST (SPECIAL-USE) "" "*"  — return only mailboxes with a SPECIAL-USE attribute.
	specialUseOnly := false
	rest := strings.TrimLeft(args, " ")
	if strings.HasPrefix(rest, "(") {
		end := strings.Index(rest, ")")
		if end > 0 {
			selOpts := strings.ToUpper(rest[1:end])
			if strings.Contains(selOpts, "SPECIAL-USE") {
				specialUseOnly = true
			}
			rest = strings.TrimLeft(rest[end+1:], " ")
		}
	}

	listArgs := splitArgs(rest)
	// LIST "" "" → return hierarchy delimiter only
	if len(listArgs) >= 2 && unquote(listArgs[1]) == "" && !specialUseOnly {
		c.write("* LIST (\\Noselect) \"/\" \"\"\r\n")
		c.write(fmt.Sprintf("%s OK LIST completed\r\n", tag))
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
		flags := mailboxFlags(box)
		if specialUseOnly && !strings.Contains(flags, "\\Sent") &&
			!strings.Contains(flags, "\\Drafts") && !strings.Contains(flags, "\\Trash") &&
			!strings.Contains(flags, "\\Junk") && !strings.Contains(flags, "\\Archive") &&
			!strings.Contains(flags, "\\All") && !strings.Contains(flags, "\\Flagged") {
			continue
		}
		c.write(fmt.Sprintf("* LIST (%s) \"/\" %s\r\n", flags, quoteString(box)))
	}
	c.write(fmt.Sprintf("%s OK LIST completed\r\n", tag))
}

// mailboxFlags returns the IMAP LIST flags for a mailbox name, including
// RFC 6154 SPECIAL-USE attributes that Apple Mail / iOS rely on to map
// the account's Sent / Drafts / Trash / Junk folders. Without these flags,
// iOS Mail refuses to fully sync the account.
func mailboxFlags(box string) string {
	flags := "\\HasNoChildren"
	switch strings.ToLower(box) {
	case "sent", "sent messages":
		flags += " \\Sent"
	case "drafts":
		flags += " \\Drafts"
	case "trash", "deleted messages":
		flags += " \\Trash"
	case "junk", "spam":
		flags += " \\Junk"
	case "archive":
		flags += " \\Archive"
	}
	return flags
}

func (c *Connection) cmdLsub(tag, args string) {
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
		c.write(fmt.Sprintf("* LSUB (%s) \"/\" %s\r\n", mailboxFlags(box), quoteString(box)))
	}
	c.write(fmt.Sprintf("%s OK LSUB completed\r\n", tag))
}

func (c *Connection) cmdStatus(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	parts := strings.SplitN(args, " ", 2)
	mailbox := unquote(parts[0])
	if !c.requireMailboxName(tag, mailbox) {
		return
	}

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

	c.write(fmt.Sprintf("* STATUS %s (%s)\r\n", quoteString(mailbox), strings.Join(result, " ")))
	c.write(fmt.Sprintf("%s OK STATUS completed\r\n", tag))
}

func (c *Connection) cmdCreate(tag, args string) {
	if !c.requireAuth(tag) {
		return
	}

	mailbox := unquote(strings.TrimSpace(args))
	if !c.requireMailboxName(tag, mailbox) {
		return
	}

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
	if !c.requireMailboxName(tag, mailbox) {
		return
	}

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

	oldName, newName := unquote(parts[0]), unquote(parts[1])
	if !c.requireMailboxName(tag, oldName) || !c.requireMailboxName(tag, newName) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.store.MailboxRename(ctx, c.auth, oldName, newName); err != nil {
		c.write(fmt.Sprintf("%s NO RENAME failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK RENAME completed\r\n", tag))
}

func (c *Connection) cmdUID(tag, args string) {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s BAD Invalid UID command\r\n", tag))
		return
	}

	subCmd := strings.ToUpper(parts[0])
	subArgs := parts[1]

	switch subCmd {
	case "FETCH":
		c.cmdFetch(tag, subArgs, true)
	case "STORE":
		c.cmdStore(tag, subArgs, true)
	case "SEARCH":
		c.cmdSearch(tag, subArgs)
	case "COPY":
		c.cmdCopy(tag, subArgs, true)
	default:
		c.write(fmt.Sprintf("%s BAD Unknown UID command\r\n", tag))
	}
}

func (c *Connection) cmdFetch(tag, args string, isUID bool) {
	if !c.requireMailbox(tag) {
		return
	}

	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		c.write(fmt.Sprintf("%s NO Invalid FETCH arguments\r\n", tag))
		return
	}

	seqSet := parts[0]
	dataItems := parts[1]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Step 1: Fetch only UIDs to build sequence number map (lightweight)
	allUIDs, err := c.store.MessageUIDs(ctx, c.auth, c.mailbox)
	if err != nil {
		c.write(fmt.Sprintf("%s NO FETCH failed\r\n", tag))
		return
	}

	// Step 2: Determine which UIDs to fetch based on requested range.
	// UIDs are assigned globally across all mailboxes, so sequence positions
	// and UID values diverge whenever messages arrive in other mailboxes.
	// seqSetToUIDs handles both UID-mode and sequence-mode correctly.
	seqMap := make(map[int64]int) // UID → sequence number
	for i, uid := range allUIDs {
		seqMap[uid] = i + 1
	}
	targetUIDs := seqSetToUIDs(seqSet, allUIDs, isUID)

	// Step 3: Fetch only the requested messages with full body
	for _, uid := range targetUIDs {
		messages, err := c.store.MessageFetch(ctx, c.auth, c.mailbox, uid, uid)
		if err != nil || len(messages) == 0 {
			continue
		}
		c.write(fmt.Sprintf("* %d FETCH (", seqMap[uid]))
		c.writeFetchItems(dataItems, &messages[0], newRawView(c, &messages[0]), isUID)
		c.write(")\r\n")
	}
	c.write(fmt.Sprintf("%s OK FETCH completed\r\n", tag))
}

func (c *Connection) writeFetchItems(items string, msg *storage.MessageRow, rv *rawView, isUID bool) {
	upper := strings.ToUpper(items)

	if isUID || strings.Contains(upper, "UID") {
		c.write(fmt.Sprintf("UID %d ", msg.UID))
	}
	if strings.Contains(upper, "FLAGS") {
		flags := parseJSONFlags(msg.Flags.String)
		c.write(fmt.Sprintf("FLAGS (%s) ", strings.Join(flags, " ")))
	}
	if strings.Contains(upper, "INTERNALDATE") {
		c.write(fmt.Sprintf("INTERNALDATE \"%s\" ", formatInternalDate(msg.Date.String)))
	}
	if strings.Contains(upper, "RFC822.SIZE") || strings.Contains(upper, "RFC822") {
		// The stored message has a size on disk; only a synthesized body has
		// to be built to be measured.
		if rv.available() {
			c.write(fmt.Sprintf("RFC822.SIZE %d ", rv.octets()))
		} else {
			c.write(fmt.Sprintf("RFC822.SIZE %d ", len(buildFullBody(msg))))
		}
	}
	if strings.Contains(upper, "ENVELOPE") {
		c.writeEnvelope(msg)
	}
	if strings.Contains(upper, "BODYSTRUCTURE") {
		c.writeBodyStructure(msg, rv)
	}

	// RFC822 full message fetch (not just SIZE)
	if strings.Contains(upper, "RFC822") && !strings.Contains(upper, "RFC822.SIZE") && !strings.Contains(upper, "RFC822.HEADER") {
		body := rv.bytesAll()
		if body == nil {
			body = []byte(buildFullBody(msg))
		}
		c.write(fmt.Sprintf("RFC822 {%d}\r\n%s", len(body), body))
	}

	// BODY / BODY.PEEK handling — check for BODY[ or BODY.PEEK[ pattern
	// to avoid conflict with BODYSTRUCTURE keyword.
	if strings.Contains(upper, "BODY[") || strings.Contains(upper, "BODY.PEEK[") {
		for _, sel := range findBodySelectors(items) {
			c.writeBodySection(sel, msg, rv)
		}
	}
}

// findBodySelectors returns each BODY[...] / BODY.PEEK[...] selector found in
// items, including any trailing <origin.count> partial range.
func findBodySelectors(items string) []string {
	var out []string
	upper := strings.ToUpper(items)
	i := 0
	for i < len(items) {
		rest := upper[i:]
		aIdx, bIdx := strings.Index(rest, "BODY["), strings.Index(rest, "BODY.PEEK[")
		var start int
		switch {
		case aIdx < 0 && bIdx < 0:
			return out
		case bIdx < 0 || (aIdx >= 0 && aIdx < bIdx):
			start = i + aIdx
		default:
			start = i + bIdx
		}
		bracket := strings.Index(items[start:], "[")
		if bracket < 0 {
			return out
		}
		bracket += start
		closeBracket := strings.Index(items[bracket+1:], "]")
		if closeBracket < 0 {
			return out
		}
		end := bracket + 1 + closeBracket + 1
		if end < len(items) && items[end] == '<' {
			if gt := strings.Index(items[end:], ">"); gt > 0 {
				end += gt + 1
			}
		}
		out = append(out, items[start:end])
		i = end
	}
	return out
}

// writeBodySection handles BODY[section] and BODY.PEEK[section] requests.
func (c *Connection) writeBodySection(items string, msg *storage.MessageRow, rv *rawView) {
	// Parse partial range: BODY[section]<origin.count>
	// RFC 3501 §6.4.5: partial fetch returns a substring of the section
	var partialOrigin, partialCount int64
	hasPartial := false
	partialStr := ""

	// Find <origin.count> after the closing ]
	closeBracket := strings.LastIndex(items, "]")
	if closeBracket >= 0 && closeBracket < len(items)-1 {
		rest := items[closeBracket+1:]
		if strings.HasPrefix(rest, "<") && strings.HasSuffix(rest, ">") {
			partialStr = rest[1 : len(rest)-1]
			dotIdx := strings.Index(partialStr, ".")
			if dotIdx >= 0 {
				origin, oErr := strconv.ParseInt(partialStr[:dotIdx], 10, 64)
				count, cErr := strconv.ParseInt(partialStr[dotIdx+1:], 10, 64)
				// RFC 3501 §9 allows only non-negative numbers here; a malformed
				// or negative range would otherwise slice the body out of bounds.
				if oErr == nil && cErr == nil && origin >= 0 && count >= 0 {
					partialOrigin, partialCount = origin, count
					hasPartial = true
				}
			}
		}
	}

	// Extract section specifier from between [ and ]
	sectionStart := strings.Index(items, "[")
	sectionEnd := strings.LastIndex(items, "]")
	section := ""
	if sectionStart >= 0 && sectionEnd > sectionStart {
		section = items[sectionStart+1 : sectionEnd]
	}

	// Build full content for the requested section
	var content string
	upperSection := strings.ToUpper(section)

	// The verbatim message answers every section exactly as transmitted,
	// including attachment parts the synthesized fallback below cannot
	// represent at all.
	if rv.available() {
		if data, ok := rv.section(section); ok {
			c.writeSectionResult(section, string(data), hasPartial, partialOrigin, partialCount)
			return
		}
		// A section the message does not contain is answered with an empty
		// string rather than a synthesized substitute, which would be a
		// different message than the one BODYSTRUCTURE described.
		c.writeSectionResult(section, "", hasPartial, partialOrigin, partialCount)
		return
	}

	switch {
	case strings.HasPrefix(upperSection, "HEADER.FIELDS"):
		pStart := strings.Index(upperSection, "(")
		pEnd := strings.Index(upperSection, ")")
		var wantFields []string
		if pStart >= 0 && pEnd > pStart {
			wantFields = strings.Fields(upperSection[pStart+1 : pEnd])
		}
		content = buildFilteredHeaders(msg, wantFields) + "\r\n"

	case upperSection == "HEADER":
		hasHTML := msg.HTML.Valid && msg.HTML.String != "" && msg.HTML.String != "0"
		hasText := msg.Text.Valid && msg.Text.String != "" && msg.Text.String != "0"
		rawHeaders := buildRawHeaders(msg)
		var sb strings.Builder
		if hasHTML && hasText {
			writeHeadersWithContentType(&sb, rawHeaders, fmt.Sprintf("multipart/alternative; boundary=\"----=_ODAC_%d\"", msg.UID))
		} else if hasHTML {
			writeHeadersWithContentType(&sb, rawHeaders, "text/html; charset=\"UTF-8\"")
		} else {
			writeHeadersWithContentType(&sb, rawHeaders, "text/plain; charset=\"UTF-8\"")
		}
		content = sb.String() + "\r\n"

	case upperSection == "TEXT":
		hasHTML := msg.HTML.Valid && msg.HTML.String != "" && msg.HTML.String != "0"
		hasText := msg.Text.Valid && msg.Text.String != "" && msg.Text.String != "0"
		if hasHTML && hasText {
			boundary := fmt.Sprintf("----=_ODAC_%d", msg.UID)
			var bodySB strings.Builder
			bodySB.WriteString("--" + boundary + "\r\n")
			bodySB.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
			bodySB.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
			bodySB.WriteString(msg.Text.String)
			bodySB.WriteString("\r\n--" + boundary + "\r\n")
			bodySB.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
			bodySB.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
			bodySB.WriteString(msg.HTML.String)
			bodySB.WriteString("\r\n--" + boundary + "--\r\n")
			content = bodySB.String()
		} else if hasHTML {
			content = msg.HTML.String
		} else if hasText {
			content = msg.Text.String
		}

	default:
		// RFC 3501 §6.4.5: numeric section selectors for multipart messages.
		// For multipart/alternative we expose part 1 = text/plain, part 2 = text/html.
		// Apple Mail / iOS request BODY[1], BODY[2], BODY[1.MIME], BODY[2.MIME] after
		// reading BODYSTRUCTURE; returning the full message here makes them render
		// the body as empty.
		hasHTML := msg.HTML.Valid && msg.HTML.String != "" && msg.HTML.String != "0"
		hasText := msg.Text.Valid && msg.Text.String != "" && msg.Text.String != "0"
		switch upperSection {
		case "1":
			if hasHTML && hasText {
				content = msg.Text.String
			} else if hasText {
				content = msg.Text.String
			} else if hasHTML {
				content = msg.HTML.String
			}
		case "2":
			if hasHTML && hasText {
				content = msg.HTML.String
			}
		case "1.MIME":
			if hasHTML && hasText {
				content = "Content-Type: text/plain; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n"
			} else if hasText {
				content = "Content-Type: text/plain; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n"
			} else if hasHTML {
				content = "Content-Type: text/html; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n"
			}
		case "2.MIME":
			if hasHTML && hasText {
				content = "Content-Type: text/html; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n"
			}
		default:
			content = buildFullBody(msg)
		}
	}

	c.writeSectionResult(section, content, hasPartial, partialOrigin, partialCount)
}

// writeSectionResult emits one BODY[section] response, applying the partial
// range when the client asked for one.
func (c *Connection) writeSectionResult(section, content string, hasPartial bool, partialOrigin, partialCount int64) {
	if !hasPartial {
		c.write(fmt.Sprintf("BODY[%s] {%d}\r\n%s ", section, len(content), content))
		return
	}

	contentBytes := []byte(content)
	total := int64(len(contentBytes))
	if partialOrigin >= total {
		content = ""
	} else {
		end := partialOrigin + partialCount
		// end < partialOrigin catches int64 overflow on an absurd count.
		if end > total || end < partialOrigin {
			end = total
		}
		content = string(contentBytes[partialOrigin:end])
	}

	// RFC 3501 §7.4.2: response includes BODY[section]<origin> with the origin octet
	c.write(fmt.Sprintf("BODY[%s]<%d> {%d}\r\n%s ", section, partialOrigin, len(content), content))
}

// buildFilteredHeaders returns only the requested header fields from the message.
func buildFilteredHeaders(msg *storage.MessageRow, wantFields []string) string {
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

	// Build a set of wanted field names (lowercase)
	want := make(map[string]bool, len(wantFields))
	for _, f := range wantFields {
		want[strings.ToLower(f)] = true
	}

	// For Content-Type, we need to return the corrected version
	needsCT := want["content-type"]

	var sb strings.Builder
	skipContinuation := false
	for _, l := range lines {
		lower := strings.ToLower(l.Key)

		// Skip content-type — we'll add corrected version below
		if lower == "content-type" || lower == "content-transfer-encoding" {
			skipContinuation = true
			continue
		}

		if skipContinuation && (strings.HasPrefix(l.Line, " ") || strings.HasPrefix(l.Line, "\t")) {
			continue
		}
		skipContinuation = false

		if want[lower] {
			sb.WriteString(l.Line)
			sb.WriteString("\r\n")
		}
	}

	// Add corrected Content-Type if requested
	if needsCT {
		hasHTML := msg.HTML.Valid && msg.HTML.String != "" && msg.HTML.String != "0"
		hasText := msg.Text.Valid && msg.Text.String != "" && msg.Text.String != "0"
		if hasHTML && hasText {
			sb.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"----=_ODAC_%d\"\r\n", msg.UID))
		} else if hasHTML {
			sb.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
		} else {
			sb.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		}
	}

	return sb.String()
}

func (c *Connection) writeEnvelope(msg *storage.MessageRow) {
	// RFC 3501 §7.4.2: ENVELOPE date is the RFC 5322 origination date string,
	// e.g. "Thu, 29 Apr 2026 17:21:00 +0000". ISO 8601 confuses Apple Mail / iOS.
	date := formatEnvelopeDate(msg.Date.String)
	subject := escapeIMAPString(msg.Subject.String)

	fromAddrs := parseMailboxJSON(msg.From.String)
	toAddrs := parseMailboxJSON(msg.To.String)

	// RFC 3501 ENVELOPE: (date subject from sender reply-to to cc bcc in-reply-to message-id)
	// sender and reply-to default to from if not present
	c.write(fmt.Sprintf("ENVELOPE (\"%s\" \"%s\" %s %s %s %s NIL NIL NIL \"%s\") ",
		date, subject, fromAddrs, fromAddrs, fromAddrs, toAddrs, escapeIMAPString(msg.MessageID.String)))
}

// formatEnvelopeDate converts a stored timestamp to RFC 5322 origination-date
// format expected by IMAP ENVELOPE (e.g. "Thu, 29 Apr 2026 17:21:00 +0000").
func formatEnvelopeDate(stored string) string {
	const envFmt = "Mon, 02 Jan 2006 15:04:05 -0700"
	if stored == "" {
		return time.Now().UTC().Format(envFmt)
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05Z",
		"2006-01-02 15:04:05",
		envFmt,
		time.RFC1123Z,
		time.RFC1123,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, stored); err == nil {
			return t.UTC().Format(envFmt)
		}
	}
	return stored
}

// parseMailboxJSON converts the Node.js mailparser JSON format
// {"value":[{"address":"user@example.com","name":"User Name"}]}
// into RFC 3501 IMAP address format: ((name NIL user host))
func parseMailboxJSON(jsonStr string) string {
	if jsonStr == "" {
		return "NIL"
	}

	type addrEntry struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	}
	type addrWrapper struct {
		Value []addrEntry `json:"value"`
	}

	var wrapper addrWrapper
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		// Try as raw address string
		if strings.Contains(jsonStr, "@") {
			parts := strings.SplitN(jsonStr, "@", 2)
			return fmt.Sprintf("((\"%s\" NIL \"%s\" \"%s\"))", "", parts[0], parts[1])
		}
		return "NIL"
	}

	if len(wrapper.Value) == 0 {
		return "NIL"
	}

	var addrs []string
	for _, entry := range wrapper.Value {
		name := escapeIMAPString(entry.Name)
		parts := strings.SplitN(entry.Address, "@", 2)
		if len(parts) != 2 {
			continue
		}
		addrs = append(addrs, fmt.Sprintf("(\"%s\" NIL \"%s\" \"%s\")", name, parts[0], parts[1]))
	}

	if len(addrs) == 0 {
		return "NIL"
	}

	return "(" + strings.Join(addrs, "") + ")"
}

func escapeIMAPString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

func (c *Connection) writeBodyStructure(msg *storage.MessageRow, rv *rawView) {
	// A structure derived from the real message is what lets a client know an
	// attachment exists; the synthesized form below can only ever describe the
	// text parts recovered by the parser.
	if rv.available() {
		if tree := rv.parts(); tree != nil {
			c.write("BODYSTRUCTURE " + tree.BodyStructure(rv.bytesAll()) + " ")
			return
		}
	}

	hasText := msg.Text.Valid && msg.Text.String != "" && msg.Text.String != "0"
	hasHTML := msg.HTML.Valid && msg.HTML.String != "" && msg.HTML.String != "0"

	// RFC 3501 §7.4.2 / §9 body-type-text:
	//   (type subtype params id desc encoding size LINES md5 disposition language)
	// LINES is REQUIRED for TEXT bodies — strict clients (Apple Mail, iOS) drop
	// messages whose BODYSTRUCTURE has NIL where a line count is mandated.
	if hasText && hasHTML {
		textSize := len(msg.Text.String)
		textLines := countLines(msg.Text.String)
		htmlSize := len(msg.HTML.String)
		htmlLines := countLines(msg.HTML.String)
		boundary := fmt.Sprintf("----=_ODAC_%d", msg.UID)
		c.write(fmt.Sprintf("BODYSTRUCTURE ((\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"8BIT\" %d %d NIL NIL NIL)(\"TEXT\" \"HTML\" (\"CHARSET\" \"UTF-8\") NIL NIL \"8BIT\" %d %d NIL NIL NIL) \"ALTERNATIVE\" (\"BOUNDARY\" \"%s\") NIL NIL) ", textSize, textLines, htmlSize, htmlLines, boundary))
	} else if hasHTML {
		c.write(fmt.Sprintf("BODYSTRUCTURE (\"TEXT\" \"HTML\" (\"CHARSET\" \"UTF-8\") NIL NIL \"8BIT\" %d %d NIL NIL NIL) ", len(msg.HTML.String), countLines(msg.HTML.String)))
	} else {
		size := 0
		lines := 0
		if hasText {
			size = len(msg.Text.String)
			lines = countLines(msg.Text.String)
		}
		c.write(fmt.Sprintf("BODYSTRUCTURE (\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"8BIT\" %d %d NIL NIL NIL) ", size, lines))
	}
}

// formatInternalDate converts a stored timestamp into the RFC 3501 §9
// date-time format expected by IMAP clients: "DD-MMM-YYYY HH:MM:SS ±HHMM".
// Apple Mail / iOS Mail strictly parse this field; ISO 8601 ("2026-04-29T17:21:00Z")
// is rejected and the message ends up hidden in the list view.
func formatInternalDate(stored string) string {
	const imapFmt = "02-Jan-2006 15:04:05 -0700"
	if stored == "" {
		return time.Now().UTC().Format(imapFmt)
	}
	// Already in IMAP format? (heuristic: third char is '-' and contains a month name spelled out)
	if len(stored) >= 11 && stored[2] == '-' && stored[6] == '-' {
		return stored
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05Z",
		"2006-01-02 15:04:05",
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, stored); err == nil {
			return t.UTC().Format(imapFmt)
		}
	}
	return time.Now().UTC().Format(imapFmt)
}

// countLines returns the number of text lines in a body part per RFC 3501 §7.4.2.
// A trailing line without a terminator still counts as a line.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func (c *Connection) cmdStore(tag, args string, isUID bool) {
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
	flags := storage.CanonicalFlags(strings.Fields(strings.Trim(flagStr, "()")))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	allUIDs, err := c.store.MessageUIDs(ctx, c.auth, c.mailbox)
	if err != nil {
		c.write(fmt.Sprintf("%s NO STORE failed\r\n", tag))
		return
	}
	uids := seqSetToUIDs(seqSet, allUIDs, isUID)

	if err := c.store.MessageStoreFlags(ctx, c.auth, uids, action, flags); err != nil {
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

	// Only UIDs and flags are consulted below, so only those are read.
	messages, err := c.store.MessageFlags(ctx, c.auth, c.mailbox)
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

	// Reader goroutine: receive DONE (or detect connection close).
	// One-shot signaling — IDLE only accepts DONE, no other commands.
	doneCh := make(chan struct{})
	errCh := make(chan struct{})
	go func() {
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Minute))
		line, err := c.reader.ReadString('\n')
		if err != nil {
			close(errCh)
			return
		}
		if strings.ToUpper(strings.TrimSpace(line)) == "DONE" {
			close(doneCh)
			return
		}
		close(errCh)
	}()

	// Poll the mailbox every 5s and push EXISTS/RECENT deltas to the client.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.pushMailboxUpdates()
		case <-doneCh:
			c.write(fmt.Sprintf("%s OK IDLE terminated\r\n", tag))
			return
		case <-errCh:
			return
		}
	}
}

func (c *Connection) cmdCopy(tag, args string, isUID bool) {
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
	if !c.requireMailboxName(tag, targetMailbox) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if isUID {
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
		if uidMax == 0 {
			uidMax = 1<<63 - 1
		}
		if err := c.store.MessageCopy(ctx, c.auth, uidMin, uidMax, c.mailbox, targetMailbox); err != nil {
			c.write(fmt.Sprintf("%s NO COPY failed\r\n", tag))
			return
		}
	} else {
		allUIDs, err := c.store.MessageUIDs(ctx, c.auth, c.mailbox)
		if err != nil {
			c.write(fmt.Sprintf("%s NO COPY failed\r\n", tag))
			return
		}
		for _, uid := range seqSetToUIDs(seqSet, allUIDs, false) {
			if err := c.store.MessageCopy(ctx, c.auth, uid, uid, c.mailbox, targetMailbox); err != nil {
				c.write(fmt.Sprintf("%s NO COPY failed\r\n", tag))
				return
			}
		}
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
	if !c.requireMailboxName(tag, mailbox) {
		return
	}

	// The ceiling matches SMTP's, so a message the server accepted for delivery
	// can always be copied back into a mailbox by the client.
	maxLiteralSize := config.MaxMessageBytes()

	flags := storage.EncodeFlags(storage.CanonicalFlags(appendFlagGroup(parts[1:])))
	var literalSize int64
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			literalSize, _ = strconv.ParseInt(p[1:len(p)-1], 10, 64)
		}
	}

	if literalSize <= 0 || literalSize > maxLiteralSize {
		c.write(fmt.Sprintf("%s NO APPEND literal size invalid or exceeds %d bytes\r\n", tag, maxLiteralSize))
		return
	}

	c.write("+ Ready for literal data\r\n")

	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	buf := make([]byte, literalSize)
	n, err := io.ReadFull(c.reader, buf)
	if err != nil {
		c.write(fmt.Sprintf("%s NO APPEND failed\r\n", tag))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// An appended message is a complete RFC 5322 message, so it is stored the
	// same way a delivered one is: the verbatim bytes are the record and the
	// columns are derived from them. Writing the raw message into the html
	// column instead, as this used to, loses every attachment on a saved draft
	// and leaves the client with no envelope to list it by.
	msg := &storage.MessageRow{
		Email:   c.auth,
		Flags:   toNullString(flags),
		Mailbox: mailbox,
		RawRef:  toNullString(c.storeRaw(buf[:n])),
	}
	message.Parse(buf[:n]).Apply(msg)

	if err := c.store.MessageStore(ctx, msg); err != nil {
		log.Printf("[IMAP] APPEND to %q for %q failed: %v", mailbox, c.auth, err)
		c.write(fmt.Sprintf("%s NO APPEND failed\r\n", tag))
		return
	}
	c.write(fmt.Sprintf("%s OK APPEND completed\r\n", tag))
}

// storeRaw writes the verbatim message to the blob store and returns its
// reference, or an empty string when no store is configured or the write fails.
// A failed blob write must not fail the APPEND: the message still lands in the
// mailbox through the derived columns, on the same fallback path that serves
// rows delivered before the blob store existed.
func (c *Connection) storeRaw(raw []byte) string {
	if c.blobs == nil {
		return ""
	}
	ref, err := c.blobs.Put(raw)
	if err != nil {
		log.Printf("[IMAP] Failed to store raw message for %q: %v", c.auth, err)
		return ""
	}
	return ref
}

func (c *Connection) write(data string) {
	c.conn.SetWriteDeadline(time.Now().Add(commandTimeout))
	c.conn.Write([]byte(data))
	if imapDebug {
		preview := data
		if len(preview) > 200 {
			preview = preview[:200] + "...(truncated)"
		}
		preview = strings.ReplaceAll(preview, "\r\n", "\\r\\n")
		log.Printf("[IMAP] S->C %s: %s", c.conn.RemoteAddr(), preview)
	}
}

// --- Helpers ---

// appendFlagGroup extracts the parenthesized flag list from APPEND arguments.
// splitArgs breaks on spaces without honoring parentheses, so `(\Seen \Draft)`
// arrives as several tokens and has to be rejoined before it can be parsed;
// reading only the first token silently dropped every flag but one.
func appendFlagGroup(parts []string) []string {
	start := -1
	for i, p := range parts {
		if strings.HasPrefix(p, "(") {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	end := start
	for end < len(parts)-1 && !strings.HasSuffix(parts[end], ")") {
		end++
	}
	group := strings.Join(parts[start:end+1], " ")
	return strings.Fields(strings.Trim(group, "()"))
}

func splitArgs(s string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false
	escaped := false

	for _, ch := range s {
		switch {
		case escaped:
			// RFC 3501: inside a quoted string a backslash escapes the next
			// character (used for literal '"' and '\'). Preserve both runes raw
			// so token boundaries stay correct; unquote decodes them later.
			current.WriteRune(ch)
			escaped = false
		case ch == '\\' && inQuote:
			current.WriteRune(ch)
			escaped = true
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

// unquote decodes an RFC 3501 quoted-string token, resolving "\x" -> "x".
// A token only counts as quoted when the *unescaped* closing quote is its last
// character: `"a\"` is an unterminated string, not the value `a`, and is
// returned untouched so the caller rejects it instead of acting on a guess.
func unquote(s string) string {
	if len(s) < 2 || s[0] != '"' {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	escaped := false
	for i, ch := range s[1:] {
		switch {
		case escaped:
			b.WriteRune(ch)
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			if i+2 == len(s) {
				return b.String()
			}
			return s
		default:
			b.WriteRune(ch)
		}
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
	// language.Und keeps casing locale-independent (matching the old strings.Title
	// behavior and avoiding locale-specific pitfalls such as the Turkish dotless-i);
	// NoLower preserves any existing casing in the rest of the word. cases.Caser is
	// not safe for concurrent use, so build one per call rather than sharing it.
	titler := cases.Title(language.Und, cases.NoLower)
	var flags []string
	for _, f := range raw {
		flags = append(flags, "\\"+titler.String(f))
	}
	return flags
}

// reassemble them into a properly formatted MIME message that mail clients can render.
func buildFullBody(msg *storage.MessageRow) string {
	hasHTML := msg.HTML.Valid && msg.HTML.String != "" && msg.HTML.String != "0"
	hasText := msg.Text.Valid && msg.Text.String != "" && msg.Text.String != "0"

	// Detect if html/text field contains a raw RFC 2822 message (legacy/broken storage).
	// If the content starts with RFC 2822 headers (e.g., "Received:", "From:", "To:"),
	// it was stored as raw message data — return it as-is since it's already a valid message.
	if hasHTML && !hasText && isRawMessage(msg.HTML.String) {
		return msg.HTML.String
	}
	if hasText && !hasHTML && isRawMessage(msg.Text.String) {
		return msg.Text.String
	}

	rawHeaders := buildRawHeaders(msg)

	var sb strings.Builder

	if hasHTML && hasText {
		boundary := fmt.Sprintf("----=_ODAC_%d", msg.UID)
		writeHeadersWithContentType(&sb, rawHeaders, "multipart/alternative; boundary=\""+boundary+"\"")
		sb.WriteString("\r\n")
		sb.WriteString("--" + boundary + "\r\n")
		sb.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		sb.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		sb.WriteString(msg.Text.String)
		sb.WriteString("\r\n--" + boundary + "\r\n")
		sb.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
		sb.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		sb.WriteString(msg.HTML.String)
		sb.WriteString("\r\n--" + boundary + "--\r\n")
	} else if hasHTML {
		writeHeadersWithContentType(&sb, rawHeaders, "text/html; charset=\"UTF-8\"")
		sb.WriteString("\r\n")
		sb.WriteString(msg.HTML.String)
	} else if hasText {
		writeHeadersWithContentType(&sb, rawHeaders, "text/plain; charset=\"UTF-8\"")
		sb.WriteString("\r\n")
		sb.WriteString(msg.Text.String)
	} else {
		sb.WriteString(rawHeaders)
		sb.WriteString("\r\n")
	}

	return sb.String()
}

// isRawMessage detects if content is a raw RFC 2822 message (has headers at the start).
func isRawMessage(content string) bool {
	// Check first few lines for common RFC 2822 header patterns
	firstLine := content
	if idx := strings.Index(content, "\n"); idx > 0 {
		firstLine = content[:idx]
	}
	firstLine = strings.TrimSpace(firstLine)

	headerPrefixes := []string{
		"received:", "from:", "to:", "subject:", "date:",
		"mime-version:", "content-type:", "dkim-signature:",
		"message-id:", "return-path:", "delivered-to:",
	}
	lower := strings.ToLower(firstLine)
	for _, prefix := range headerPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// buildRawHeaders extracts raw header lines from the DB JSON, excluding Content-Type.
func buildRawHeaders(msg *storage.MessageRow) string {
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

// writeHeadersWithContentType writes headers, replacing the original Content-Type
// with the correct one for the reconstructed body. Also strips Content-Transfer-Encoding
// since DB content is already decoded.
func writeHeadersWithContentType(sb *strings.Builder, rawHeaders, contentType string) {
	skipContinuation := false
	for _, line := range strings.Split(rawHeaders, "\r\n") {
		if line == "" {
			continue
		}

		lower := strings.ToLower(line)

		// Skip Content-Type and Content-Transfer-Encoding headers (we replace them)
		if strings.HasPrefix(lower, "content-type:") || strings.HasPrefix(lower, "content-transfer-encoding:") {
			skipContinuation = true
			continue
		}

		// Skip continuation lines (start with whitespace) of skipped headers
		if skipContinuation && (line[0] == ' ' || line[0] == '\t') {
			continue
		}
		skipContinuation = false

		sb.WriteString(line)
		sb.WriteString("\r\n")
	}

	// Add our correct Content-Type
	sb.WriteString("Content-Type: " + contentType + "\r\n")
}

// authComparePassword wraps the auth package to avoid circular imports.
func authComparePassword(password, storedHash string) (bool, error) {
	return auth.ComparePassword(password, storedHash)
}

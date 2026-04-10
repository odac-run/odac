package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"

	"odac-mail/auth"
	"odac-mail/config"
	"odac-mail/storage"
)

const (
	idleTimeout    = 30 * time.Minute
	commandTimeout = 30 * time.Second
	maxCommandSize = 8192
	maxLineSize    = 64 * 1024
)

var permanentFlags = []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`, `\*`}
var capabilities = "IMAP4rev1 AUTH=PLAIN IDLE"

// Connection represents a single IMAP client session with its state machine.
type Connection struct {
	auth      string // Authenticated email (empty = not authenticated)
	conn      net.Conn
	firewall  *auth.Firewall
	getConfig func() config.Config
	mailbox   string // Currently selected mailbox
	reader    *bufio.Reader
	store     *storage.Store
}

// NewConnection creates a new IMAP connection handler.
func NewConnection(conn net.Conn, store *storage.Store, fw *auth.Firewall, getConfig func() config.Config) *Connection {
	return &Connection{
		conn:      conn,
		firewall:  fw,
		getConfig: getConfig,
		reader:    bufio.NewReaderSize(conn, maxLineSize),
		store:     store,
	}
}

// Serve runs the IMAP protocol loop: greeting → command processing → logout.
func (c *Connection) Serve() {
	// Send greeting
	c.write("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] IMAP4rev1 Server Ready\r\n")

	for {
		c.conn.SetReadDeadline(time.Now().Add(idleTimeout))

		line, err := c.reader.ReadString('\n')
		if err != nil {
			return // Connection closed or timeout
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" || len(line) > maxCommandSize {
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			continue
		}

		tag := parts[0]
		cmd := strings.ToUpper(parts[1])
		args := ""
		if len(parts) > 2 {
			args = parts[2]
		}

		switch cmd {
		case "CAPABILITY":
			c.cmdCapability(tag)
		case "NOOP":
			c.write(fmt.Sprintf("%s OK NOOP completed\r\n", tag))
		case "LOGOUT":
			c.cmdLogout(tag)
			return
		case "LOGIN":
			c.cmdLogin(tag, args)
		case "AUTHENTICATE":
			c.cmdAuthenticate(tag, args)
		case "SELECT":
			c.cmdSelect(tag, args)
		case "EXAMINE":
			c.cmdExamine(tag, args)
		case "LIST":
			c.cmdList(tag)
		case "LSUB":
			c.cmdLsub(tag)
		case "STATUS":
			c.cmdStatus(tag, args)
		case "CREATE":
			c.cmdCreate(tag, args)
		case "DELETE":
			c.cmdDelete(tag, args)
		case "RENAME":
			c.cmdRename(tag, args)
		case "FETCH", "UID":
			c.cmdFetch(tag, cmd, args)
		case "STORE":
			c.cmdStore(tag, args)
		case "COPY":
			c.cmdCopy(tag, args)
		case "APPEND":
			c.cmdAppend(tag, args)
		case "EXPUNGE":
			c.cmdExpunge(tag)
		case "CLOSE":
			c.cmdClose(tag)
		case "SEARCH":
			c.cmdSearch(tag, args)
		case "IDLE":
			c.cmdIdle(tag)
		case "STARTTLS":
			c.write(fmt.Sprintf("%s NO STARTTLS not available on this connection\r\n", tag))
		default:
			c.write(fmt.Sprintf("%s BAD Unknown command\r\n", tag))
		}
	}
}

func (c *Connection) requireAuth(tag string) bool {
	if c.auth == "" {
		c.write(fmt.Sprintf("%s NO Authentication required\r\n", tag))
		return false
	}
	return true
}

func (c *Connection) requireMailbox(tag string) bool {
	if !c.requireAuth(tag) {
		return false
	}
	if c.mailbox == "" {
		c.write(fmt.Sprintf("%s NO Mailbox required\r\n", tag))
		return false
	}
	return true
}

package consistency

import (
	"encoding/xml"
	"errors"
	"strings"

	"odac/internal/docker"
)

// secretFile is where a wrapped command finds its credential file: the
// dump container's /tmp, a tmpfs that goes with the container.
const secretFile = "/tmp/odac-secret"

// The wrappers are fixed scripts: what varies reaches them as arguments
// ($0 is the tool, "$@" its arguments) or on stdin, never as script text,
// so nothing a database or an app chose is ever parsed by the shell. The
// tool gets /dev/null as stdin, so nothing it reads can be the secret.
const (
	// fileScript writes stdin to secretFile, readable by this user only,
	// for tools that read credentials from a file.
	fileScript = `umask 077 && cat >` + secretFile + ` && exec "$0" "$@" </dev/null`
	// redisScript reads one line into REDISCLI_AUTH, which redis-cli and
	// its forks read; a process's environment is readable only by its
	// user, unlike its command line.
	redisScript = `IFS= read -r REDISCLI_AUTH; export REDISCLI_AUTH; exec "$0" "$@" </dev/null`
)

type secretMode int

const (
	noSecret secretMode = iota
	secretAsFile
	secretAsRedisAuth
)

// command is one invocation of a database tool.
type command struct {
	argv   []string
	env    []string
	secret []byte
	mode   secretMode
}

func (c command) spec(app App) docker.DumpSpec {
	s := docker.DumpSpec{Image: app.image(), App: app.Container, Env: c.env}
	switch {
	case c.mode == secretAsFile && c.secret != nil:
		s.Cmd = append([]string{"sh", "-c", fileScript}, c.argv...)
		s.Stdin = c.secret
	case c.mode == secretAsRedisAuth && c.secret != nil:
		s.Cmd = append([]string{"sh", "-c", redisScript}, c.argv...)
		s.Stdin = append(append([]byte(nil), c.secret...), '\n')
	default:
		s.Cmd = c.argv
	}
	return s
}

var errSecretChars = errors.New("consistency: a credential holds a line break or NUL, which no client file can carry")

func plainSecret(s string) error {
	if strings.ContainsAny(s, "\x00\r\n") {
		return errSecretChars
	}
	return nil
}

// pgpassField escapes a .pgpass field.
func pgpassField(s string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(s)
}

// cnfValue quotes a MySQL option file value.
func cnfValue(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// yamlString quotes a YAML scalar.
func yamlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// xmlText escapes XML character data.
func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

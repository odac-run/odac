package settings

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"odac/internal/backup/proto"
)

const (
	// MinKeyLength is the shortest passphrase an operator may choose. It is
	// the engine's minimum; a generated key is longer.
	MinKeyLength = 16
	maxKeyBytes  = 1024
	keyBytes     = 20

	pendingTTL = 30 * time.Minute
	maxPending = 16
)

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// generateKey returns 160 random bits as lowercase base32 in groups of
// four, which a person can read out or copy without confusing characters.
func generateKey() string {
	b := make([]byte, keyBytes)
	rand.Read(b)
	s := strings.ToLower(keyEncoding.EncodeToString(b))
	var out strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			out.WriteByte('-')
		}
		out.WriteString(s[i:min(i+4, len(s))])
	}
	return out.String()
}

// checkKey refuses a passphrase that is too short, or one that would not
// survive being copied: surrounding spaces and control characters are
// what a paste adds or drops without anyone seeing it.
func checkKey(k string) error {
	if utf8.RuneCountInString(k) < MinKeyLength {
		return invalid("A key is at least %s characters.", MinKeyLength)
	}
	if len(k) > maxKeyBytes || !utf8.ValidString(k) {
		return invalid("A key is at most %s bytes of text.", maxKeyBytes)
	}
	if strings.TrimSpace(k) != k {
		return invalid("A key cannot start or end with a space.")
	}
	for _, r := range k {
		if unicode.IsControl(r) {
			return invalid("A key cannot contain control characters.")
		}
	}
	return nil
}

// Recovery is what the operator keeps to open the backups without this
// server: a new ODAC adds the same target, enters the key and restores
// from this server's directory on it. Storage credentials are left out;
// they come from the storage provider, and the file would otherwise open
// the storage as well as the backups.
type Recovery struct {
	Format     int        `json:"format"`
	ServerID   string     `json:"serverId"`
	ServerName string     `json:"serverName,omitempty"`
	Target     TargetView `json:"target"`
	// Directory is where this server's repository lies inside the target.
	Directory string `json:"directory"`
	Key       string `json:"key"`
}

// Pending is a target waiting for the operator to confirm that they saved
// its key. Nothing is written before that.
type Pending struct {
	Token    string   `json:"token"`
	Expires  int64    `json:"expires"`
	Recovery Recovery `json:"recovery"`
}

type pendingTarget struct {
	id      string
	target  *Target
	expires time.Time
}

func (s *Settings) recovery(id string, t *Target) Recovery {
	v := s.view(id, t)
	v.Conflict = nil
	return Recovery{
		Format:     1,
		ServerID:   s.cfg.ServerID,
		ServerName: s.cfg.ServerName(),
		Target:     v,
		Directory:  s.cfg.ServerID + "/" + proto.RepositoryDir,
		Key:        t.Key,
	}
}

// PrepareTarget checks a new target and returns its key to show once,
// with a token for ConfirmTarget. The target is saved only when the
// operator confirms; until then it exists in this process alone and is
// dropped after half an hour.
func (s *Settings) PrepareTarget(ctx context.Context, spec TargetSpec) (Pending, error) {
	t, err := s.normalize(spec)
	if err != nil {
		return Pending{}, err
	}
	if spec.Key != "" {
		if err := checkKey(spec.Key); err != nil {
			return Pending{}, err
		}
		t.Key, t.KeySource = spec.Key, KeyEntered
	} else {
		t.Key, t.KeySource = generateKey(), KeyGenerated
	}
	m, err := s.read()
	if err != nil {
		return Pending{}, err
	}
	if err := checkUnique(&m, "", t); err != nil {
		return Pending{}, err
	}
	t.Location = s.locate(ctx, t)

	tok := make([]byte, 16)
	rand.Read(tok)
	p := &pendingTarget{id: newID(), target: t, expires: s.cfg.Now().Add(pendingTTL)}
	token := hex.EncodeToString(tok)

	s.mu.Lock()
	s.sweepPendingLocked()
	if len(s.pending) >= maxPending {
		s.dropOldestPendingLocked()
	}
	s.pending[token] = p
	s.mu.Unlock()
	return Pending{Token: token, Expires: p.expires.UnixMilli(), Recovery: s.recovery(p.id, t)}, nil
}

func (s *Settings) sweepPendingLocked() {
	now := s.cfg.Now()
	for tok, p := range s.pending {
		if !now.Before(p.expires) {
			s.dropPendingLocked(tok)
		}
	}
}

func (s *Settings) dropOldestPendingLocked() {
	var oldest string
	for tok, p := range s.pending {
		if oldest == "" || p.expires.Before(s.pending[oldest].expires) {
			oldest = tok
		}
	}
	if oldest != "" {
		s.dropPendingLocked(oldest)
	}
}

func (s *Settings) dropPendingLocked(tok string) {
	if p := s.pending[tok]; p != nil {
		s.owners.forget(p.id)
	}
	delete(s.pending, tok)
}

// ConfirmTarget saves a prepared target once the operator says they saved
// its key. It first runs init, which creates the repository in this
// server's directory or opens the one there with the key, so a target is
// never saved that its runs could not use. A failure keeps the token, so
// the operator can fix the cause, or adopt a directory this server wrote
// before, and confirm again.
func (s *Settings) ConfirmTarget(ctx context.Context, token string, adopt bool) (TargetView, error) {
	s.mu.Lock()
	s.sweepPendingLocked()
	p := s.pending[token]
	s.mu.Unlock()
	if p == nil {
		return TargetView{}, errorf(CodeExpired, "This target was not saved in time. Add it again.")
	}
	res, err := s.initTarget(ctx, p.id, p.target, adopt)
	if err != nil {
		return TargetView{}, err
	}

	t := *p.target
	t.SameDisk = res.SameDisk && t.Disk != nil
	t.Created = s.nowMs()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[token] != p {
		return TargetView{}, errorf(CodeExpired, "This target was not saved in time. Add it again.")
	}
	err = s.update(func(m *module) error {
		if err := checkUnique(m, p.id, &t); err != nil {
			return err
		}
		m.Targets[p.id] = &t
		return nil
	})
	if err != nil {
		return TargetView{}, err
	}
	delete(s.pending, token)
	s.cfg.Log.Log("Added backup target %s (%s, %s)", t.Name, t.kind(), t.Location)
	return s.view(p.id, &t), nil
}

// CancelTarget drops a prepared target.
func (s *Settings) CancelTarget(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPendingLocked(token)
}

// Key returns a saved target's key with what else recovery needs. It is
// how the key is shown again while the server is up, so the API keeps it
// to the operator.
func (s *Settings) Key(id string) (Recovery, error) {
	m, err := s.read()
	if err != nil {
		return Recovery{}, err
	}
	t := m.Targets[id]
	if t == nil {
		return Recovery{}, errorf(CodeNotFound, "Backup target %s not found.", id)
	}
	return s.recovery(id, t), nil
}

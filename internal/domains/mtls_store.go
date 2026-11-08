package domains

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"odac/internal/mtls"
)

// certStoreDSN keeps synchronous on FULL because a revocation lost to a power cut
// is a certificate working again. The driver only honors _pragma parameters.
const certStoreDSN = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"

const certStoreSchema = `
CREATE TABLE IF NOT EXISTS certs (
	fingerprint  TEXT PRIMARY KEY,
	ca           TEXT,
	cert         TEXT,
	created      INTEGER,
	hosts        TEXT,
	name         TEXT,
	not_after    INTEGER,
	renewed_from TEXT,
	revoked      INTEGER,
	serial       TEXT,
	spki         TEXT
);
CREATE INDEX IF NOT EXISTS certs_key ON certs(ca, spki);
CREATE INDEX IF NOT EXISTS certs_name ON certs(ca, name);`

const certColumns = "fingerprint, ca, cert, created, hosts, name, not_after, renewed_from, revoked, serial, spki"

// certStore holds issued client certificates outside the config store. A config
// module is rewritten whole under the lock every config reader takes, which cost
// 1.27s per certificate at 100k records and stopped the server for that long. It
// has its own lock, so it is safe to call while holding the config lock, but a
// scan belongs outside of it.
type certStore struct {
	db *sql.DB
}

func openCertStore(path string) (*certStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+certStoreDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(certStoreSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &certStore{db: db}, nil
}

func (s *certStore) close() error { return s.db.Close() }

type rowScanner interface {
	Scan(dest ...any) error
}

// scanCert rebuilds the record shape the certificate logic and the API replies
// use, with absent columns left out: a revocation of a certificate another CA
// issued has nothing but a fingerprint and a time.
func scanCert(row rowScanner) (string, map[string]any, error) {
	var fingerprint string
	var ca, cert, hosts, name, renewedFrom, serial, spki sql.NullString
	var created, notAfter, revoked sql.NullInt64
	if err := row.Scan(&fingerprint, &ca, &cert, &created, &hosts, &name, &notAfter, &renewedFrom, &revoked, &serial, &spki); err != nil {
		return "", nil, err
	}
	rec := map[string]any{}
	for key, value := range map[string]sql.NullString{"ca": ca, "cert": cert, "name": name, "renewedFrom": renewedFrom, "serial": serial, "spki": spki} {
		if value.Valid {
			rec[key] = value.String
		}
	}
	for key, value := range map[string]sql.NullInt64{"created": created, "notAfter": notAfter, "revoked": revoked} {
		if value.Valid {
			rec[key] = float64(value.Int64)
		}
	}
	if hosts.Valid {
		list := []string{}
		if err := json.Unmarshal([]byte(hosts.String), &list); err != nil {
			return "", nil, err
		}
		rec["hosts"] = anyList(list)
	}
	return fingerprint, rec, nil
}

func nullable(rec map[string]any, key string) any {
	value, ok := rec[key]
	if !ok {
		return nil
	}
	if number, ok := value.(float64); ok {
		return int64(number)
	}
	return value
}

func (s *certStore) get(fingerprint string) (map[string]any, error) {
	row := s.db.QueryRow("SELECT "+certColumns+" FROM certs WHERE fingerprint = ?", fingerprint)
	_, rec, err := scanCert(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

func (s *certStore) insert(fingerprint string, rec map[string]any) error {
	hosts, err := json.Marshal(stringList(rec["hosts"]))
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO certs ("+certColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		fingerprint, nullable(rec, "ca"), nullable(rec, "cert"), nullable(rec, "created"), string(hosts), nullable(rec, "name"),
		nullable(rec, "notAfter"), nullable(rec, "renewedFrom"), nullable(rec, "revoked"), nullable(rec, "serial"), nullable(rec, "spki"))
	return err
}

// byKey returns every record a public key holds under a CA, in the shape matchKey
// and liveOnSameKey read.
func (s *certStore) byKey(ca, spki string) (map[string]any, error) {
	rows, err := s.db.Query("SELECT "+certColumns+" FROM certs WHERE ca = ? AND spki = ?", ca, spki)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]any{}
	for rows.Next() {
		fingerprint, rec, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out[fingerprint] = rec
	}
	return out, rows.Err()
}

func (s *certStore) nameTaken(ca, name string) (bool, error) {
	var found int
	err := s.db.QueryRow("SELECT 1 FROM certs WHERE ca = ? AND name = ? AND revoked IS NULL LIMIT 1", ca, name).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *certStore) setHosts(fingerprint string, hosts []string) error {
	raw, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE certs SET hosts = ? WHERE fingerprint = ?", string(raw), fingerprint)
	return err
}

// revoke keeps the first revocation time, and creates a record for a certificate
// ODAC has none of, which is the only way to revoke what an external CA issued.
func (s *certStore) revoke(fingerprint string, now float64) error {
	_, err := s.db.Exec(`INSERT INTO certs (fingerprint, revoked) VALUES (?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET revoked = COALESCE(revoked, excluded.revoked)`, fingerprint, int64(now))
	return err
}

// anyLive calls match with the scope of each usable certificate of a CA until it
// reports a hit.
func (s *certStore) anyLive(ca string, now float64, match func(hosts []string) bool) (bool, error) {
	rows, err := s.db.Query("SELECT hosts FROM certs WHERE ca = ? AND revoked IS NULL AND not_after > ?", ca, int64(now))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		hosts := []string{}
		if raw.Valid {
			if err := json.Unmarshal([]byte(raw.String), &hosts); err != nil {
				return false, err
			}
		}
		if match(hosts) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// certListSelect reads dashboard rows: the record shape minus PEMs and key hashes.
const certListSelect = "SELECT fingerprint, ca, NULL, created, hosts, name, not_after, NULL, revoked, serial, NULL FROM certs"

// row returns one record as list would, or nil when there is none.
func (s *certStore) row(fingerprint string) (map[string]any, error) {
	fp, rec, err := scanCert(s.db.QueryRow(certListSelect+" WHERE fingerprint = ?", fingerprint))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec["fingerprint"] = fp
	return rec, nil
}

// list returns dashboard rows in fingerprint order, without PEMs or key hashes.
// A nil ids lists everything, including revocations with no CA.
func (s *certStore) list(ids []string) ([]map[string]any, error) {
	query := certListSelect
	args := make([]any, len(ids))
	if ids != nil {
		for i, id := range ids {
			args[i] = id
		}
		query += " WHERE ca IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	}
	rows, err := s.db.Query(query+" ORDER BY fingerprint", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		fingerprint, rec, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		rec["fingerprint"] = fingerprint
		out = append(out, rec)
	}
	return out, rows.Err()
}

// snapshot builds the proxy's view. Scopes are spliced from the stored JSON
// because decoding and re-encoding every row would be most of the cost of a push.
func (s *certStore) snapshot(now time.Time) (mtls.CertSnapshot, error) {
	rows, err := s.db.Query("SELECT fingerprint, hosts, revoked IS NOT NULL, not_after FROM certs ORDER BY fingerprint")
	if err != nil {
		return mtls.CertSnapshot{}, err
	}
	defer rows.Close()
	var scopes strings.Builder
	scopes.WriteByte('{')
	revoked := []string{}
	nowMillis := now.UnixMilli()
	for rows.Next() {
		var fingerprint string
		var hosts sql.NullString
		var gone bool
		var notAfter sql.NullInt64
		if err := rows.Scan(&fingerprint, &hosts, &gone, &notAfter); err != nil {
			return mtls.CertSnapshot{}, err
		}
		if gone {
			// Past its expiry the chain check already refuses it.
			if !notAfter.Valid || notAfter.Int64 > nowMillis {
				revoked = append(revoked, fingerprint)
			}
			continue
		}
		if scopes.Len() > 1 {
			scopes.WriteByte(',')
		}
		list := "[]"
		if hosts.Valid {
			list = hosts.String
		}
		// A fingerprint is hex by the time it gets here, so it needs no escaping.
		scopes.WriteByte('"')
		scopes.WriteString(fingerprint)
		scopes.WriteString(`":`)
		scopes.WriteString(list)
	}
	if err := rows.Err(); err != nil {
		return mtls.CertSnapshot{}, err
	}
	scopes.WriteByte('}')
	return mtls.CertSnapshot{Scopes: json.RawMessage(scopes.String()), Revoked: revoked}, nil
}

// certScope is the part of a record Prune decides on.
type certScope struct {
	ca          string
	fingerprint string
	hasHosts    bool
	hosts       []string
}

func (s *certStore) scopes() ([]certScope, error) {
	rows, err := s.db.Query("SELECT fingerprint, ca, hosts FROM certs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []certScope
	for rows.Next() {
		var entry certScope
		var ca, hosts sql.NullString
		if err := rows.Scan(&entry.fingerprint, &ca, &hosts); err != nil {
			return nil, err
		}
		entry.ca = ca.String
		if hosts.Valid {
			entry.hasHosts = true
			if err := json.Unmarshal([]byte(hosts.String), &entry.hosts); err != nil {
				return nil, err
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// applyPrune removes and trims in one transaction, so a crash cannot leave a
// deleted domain half cleared.
func (s *certStore) applyPrune(remove []string, trim map[string][]string) error {
	if len(remove) == 0 && len(trim) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, fingerprint := range remove {
		if _, err := tx.Exec("DELETE FROM certs WHERE fingerprint = ?", fingerprint); err != nil {
			return err
		}
	}
	for fingerprint, hosts := range trim {
		raw, err := json.Marshal(hosts)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE certs SET hosts = ? WHERE fingerprint = ?", string(raw), fingerprint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

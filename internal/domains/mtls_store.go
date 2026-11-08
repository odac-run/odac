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
	spki         TEXT,
	issuer_host  TEXT,
	issuer_cert  TEXT
);
CREATE INDEX IF NOT EXISTS certs_key ON certs(ca, spki);
CREATE INDEX IF NOT EXISTS certs_name ON certs(ca, name);`

// certStoreAdded are the columns a store made before them lacks. SQLite has no
// ADD COLUMN IF NOT EXISTS, so openCertStore adds whichever is missing.
var certStoreAdded = []string{"issuer_host TEXT", "issuer_cert TEXT"}

// certStoreIssuers holds this server's per-host issuers (mtls.IssuerPerHost).
// issuer_host on a certificate names the host whose issuer signed it, here or on
// another server, and issuer_cert that issuer, which a retried issue hands back
// as its chain.
const certStoreIssuers = `
CREATE INDEX IF NOT EXISTS certs_issuer ON certs(ca, issuer_host);
CREATE TABLE IF NOT EXISTS issuers (
	ca      TEXT NOT NULL,
	host    TEXT NOT NULL,
	cert    TEXT NOT NULL,
	created INTEGER,
	PRIMARY KEY (ca, host)
);`

const certColumns = "fingerprint, ca, cert, created, hosts, name, not_after, renewed_from, revoked, serial, spki, issuer_host, issuer_cert"

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
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(certStoreIssuers); err != nil {
		db.Close()
		return nil, err
	}
	return &certStore{db: db}, nil
}

func addMissingColumns(db *sql.DB) error {
	rows, err := db.Query("SELECT name FROM pragma_table_info('certs')")
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range certStoreAdded {
		name, _, _ := strings.Cut(column, " ")
		if have[name] {
			continue
		}
		if _, err := db.Exec("ALTER TABLE certs ADD COLUMN " + column); err != nil {
			return err
		}
	}
	return nil
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
	var ca, cert, hosts, name, renewedFrom, serial, spki, issuerHost, issuerCert sql.NullString
	var created, notAfter, revoked sql.NullInt64
	if err := row.Scan(&fingerprint, &ca, &cert, &created, &hosts, &name, &notAfter, &renewedFrom, &revoked, &serial, &spki, &issuerHost, &issuerCert); err != nil {
		return "", nil, err
	}
	rec := map[string]any{}
	for key, value := range map[string]sql.NullString{"ca": ca, "cert": cert, "issuerCert": issuerCert, "issuerHost": issuerHost, "name": name, "renewedFrom": renewedFrom, "serial": serial, "spki": spki} {
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
	_, err = s.db.Exec("INSERT INTO certs ("+certColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		fingerprint, nullable(rec, "ca"), nullable(rec, "cert"), nullable(rec, "created"), string(hosts), nullable(rec, "name"),
		nullable(rec, "notAfter"), nullable(rec, "renewedFrom"), nullable(rec, "revoked"), nullable(rec, "serial"), nullable(rec, "spki"),
		nullable(rec, "issuerHost"), nullable(rec, "issuerCert"))
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

// liveCount counts the certificates a CA signed that would still open something.
func (s *certStore) liveCount(ca string, now float64) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM certs WHERE ca = ? AND revoked IS NULL AND not_after > ?", ca, int64(now)).Scan(&n)
	return n, err
}

// liveNames names up to limit of the certificates liveCount counts, by name, or by
// the start of the fingerprint for one recorded without a name.
func (s *certStore) liveNames(ca string, now float64, limit int) ([]string, error) {
	rows, err := s.db.Query(`SELECT COALESCE(NULLIF(name, ''), substr(fingerprint, 1, 16)) AS label FROM certs
		WHERE ca = ? AND revoked IS NULL AND not_after > ? ORDER BY label LIMIT ?`, ca, int64(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// retireCA drops the certificates of a removed CA but keeps its revocations: an
// imported CA can be imported again, and a revoked row is what refuses the same
// certificate on a later cert.import.
func (s *certStore) retireCA(ca string) error {
	_, err := s.db.Exec("DELETE FROM certs WHERE ca = ? AND revoked IS NULL", ca)
	return err
}

// certListSelect reads dashboard rows: the record shape minus PEMs and key hashes.
const certListSelect = "SELECT fingerprint, ca, NULL, created, hosts, name, not_after, NULL, revoked, serial, NULL, NULL, NULL FROM certs"

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
// An expired certificate is left out like a revoked one: the chain check already
// refuses it, and a scope the proxy holds makes its host ask for a certificate.
// Issuers go only with a live certificate signed through them; Direct is decoded
// only for the CAs that have issuers, so a store with none pays nothing for it.
func (s *certStore) snapshot(now time.Time) (mtls.CertSnapshot, error) {
	issuers, err := s.issuers()
	if err != nil {
		return mtls.CertSnapshot{}, err
	}
	issuerCAs := map[string]bool{}
	for _, issuer := range issuers {
		issuerCAs[issuer.CA] = true
	}
	viaLive := map[string]bool{}
	direct := map[string]bool{}

	rows, err := s.db.Query("SELECT fingerprint, hosts, revoked IS NOT NULL, not_after, ca, issuer_host FROM certs ORDER BY fingerprint")
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
		var hosts, ca, issuerHost sql.NullString
		var gone bool
		var notAfter sql.NullInt64
		if err := rows.Scan(&fingerprint, &hosts, &gone, &notAfter, &ca, &issuerHost); err != nil {
			return mtls.CertSnapshot{}, err
		}
		if gone {
			// Past its expiry the chain check already refuses it.
			if !notAfter.Valid || notAfter.Int64 > nowMillis {
				revoked = append(revoked, fingerprint)
			}
			continue
		}
		if notAfter.Valid && notAfter.Int64 <= nowMillis {
			continue
		}
		if issuerHost.Valid {
			viaLive[ca.String+"\x00"+issuerHost.String] = true
		} else if issuerCAs[ca.String] && hosts.Valid {
			var list []string
			if err := json.Unmarshal([]byte(hosts.String), &list); err != nil {
				return mtls.CertSnapshot{}, err
			}
			for _, host := range list {
				direct[ca.String+"\x00"+host] = true
			}
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
	out := mtls.CertSnapshot{Scopes: json.RawMessage(scopes.String()), Revoked: revoked}
	for _, issuer := range issuers {
		key := issuer.CA + "\x00" + issuer.Host
		if !viaLive[key] {
			continue
		}
		issuer.Direct = direct[key] || direct[issuer.CA+"\x00"+mtls.WildcardOf(issuer.Host)]
		out.Issuers = append(out.Issuers, issuer)
	}
	return out, nil
}

// issuers returns every per-host issuer, without its key, which lives in a file.
func (s *certStore) issuers() ([]mtls.HostIssuer, error) {
	rows, err := s.db.Query("SELECT ca, host, cert FROM issuers ORDER BY ca, host")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mtls.HostIssuer
	for rows.Next() {
		var issuer mtls.HostIssuer
		if err := rows.Scan(&issuer.CA, &issuer.Host, &issuer.Cert); err != nil {
			return nil, err
		}
		out = append(out, issuer)
	}
	return out, rows.Err()
}

// issuer returns the certificate of this server's issuer for host under ca, or "".
func (s *certStore) issuer(ca, host string) (string, error) {
	var cert string
	err := s.db.QueryRow("SELECT cert FROM issuers WHERE ca = ? AND host = ?", ca, host).Scan(&cert)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return cert, err
}

func (s *certStore) putIssuer(ca, host, cert string, created int64) error {
	_, err := s.db.Exec(`INSERT INTO issuers (ca, host, cert, created) VALUES (?, ?, ?, ?)
		ON CONFLICT(ca, host) DO UPDATE SET cert = excluded.cert, created = excluded.created`, ca, host, cert, created)
	return err
}

func (s *certStore) deleteIssuer(ca, host string) error {
	_, err := s.db.Exec("DELETE FROM issuers WHERE ca = ? AND host = ?", ca, host)
	return err
}

// idleIssuers lists the issuers no live certificate was signed through, here or
// on another server: nothing needs their name in a handshake any more.
func (s *certStore) idleIssuers(now float64) ([]mtls.HostIssuer, error) {
	rows, err := s.db.Query(`SELECT ca, host FROM issuers i WHERE NOT EXISTS (
		SELECT 1 FROM certs c WHERE c.ca = i.ca AND c.issuer_host = i.host AND c.revoked IS NULL AND c.not_after > ?)`, int64(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mtls.HostIssuer
	for rows.Next() {
		var issuer mtls.HostIssuer
		if err := rows.Scan(&issuer.CA, &issuer.Host); err != nil {
			return nil, err
		}
		out = append(out, issuer)
	}
	return out, rows.Err()
}

// liveIssuerHosts returns the hosts some live certificate was signed for by a
// per-host issuer, the ones a record cannot leave per-host mode over.
func (s *certStore) liveIssuerHosts(now float64) ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT issuer_host FROM certs WHERE issuer_host IS NOT NULL AND revoked IS NULL AND not_after > ?", int64(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		out = append(out, host)
	}
	return out, rows.Err()
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

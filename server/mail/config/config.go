// Package config defines the data structures for mail server configuration.
// These structs map directly to the JSON payload received from the Node.js
// control plane via the Unix socket API, enabling zero-copy deserialization.
// Architecture mirrors odac-proxy/config and odac-dns/config for consistency.
package config

// Config represents the full configuration payload from Node.js.
// Pushed atomically via POST /config on the control API socket.
type Config struct {
	Accounts []Account         `json:"accounts"`
	Domains  map[string]Domain `json:"domains"`
	Hostname string            `json:"hostname"`
	SSL      SSL               `json:"ssl"`
}

// Domain represents a mail-enabled domain with its TLS and DKIM configuration.
type Domain struct {
	Cert       DomainCert `json:"cert"`
	MXEnabled  bool       `json:"mxEnabled"`
	Subdomains []string   `json:"subdomains"`
}

// DomainCert holds per-domain SSL and DKIM certificate paths.
type DomainCert struct {
	DKIM *DKIMConfig `json:"dkim,omitempty"`
	SSL  SSL         `json:"ssl"`
}

// DKIMConfig holds DKIM signing key paths and selector for a domain.
type DKIMConfig struct {
	Private  string `json:"private"`
	Public   string `json:"public"`
	Selector string `json:"selector"`
}

// SSL holds TLS key and certificate file paths.
type SSL struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Account represents a mail account synced from Node.js.
// Used for authentication lookups without direct DB access from Node.js.
type Account struct {
	Domain   string `json:"domain"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// SpamConfig holds cloud-synced anti-spam rules and DNSBL configuration.
// Designed for future Hub integration where rules are pushed periodically.
type SpamConfig struct {
	Blacklists     []string   `json:"blacklists"`
	CustomHeaders  map[string]string `json:"customHeaders"`
	Rules          []SpamRule `json:"rules"`
	ScoreThreshold float64    `json:"scoreThreshold"`
}

// SpamRule represents a single cloud-synced spam detection rule.
type SpamRule struct {
	Action  string `json:"action"`
	Enabled bool   `json:"enabled"`
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Score   float64 `json:"score"`
	Target  string `json:"target"`
}

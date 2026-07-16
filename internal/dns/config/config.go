// Package config defines the data structures for DNS zone configuration.
// These structs map directly to the JSON payload received from the Node.js
// control plane via the Unix socket API, enabling zero-copy deserialization.
package config

// Config represents the top-level DNS configuration payload sent by Node.js.
type Config struct {
	IPs   IPConfig        `json:"ips"`
	Zones map[string]Zone `json:"zones"`
}

// IPConfig holds the server's detected IP addresses for auto-populating
// A/AAAA records when explicit values resolve to loopback (127.0.0.1).
type IPConfig struct {
	IPv4 []IPEntry `json:"ipv4"`
	IPv6 []IPEntry `json:"ipv6"`
	// Primary is the preferred IPv4 address for backward compatibility.
	Primary string `json:"primary"`
}

// IPEntry represents a single detected IP address with optional PTR record.
type IPEntry struct {
	Address string `json:"address"`
	PTR     string `json:"ptr"`
	Public  bool   `json:"public"`
}

// Zone represents a DNS zone with its SOA record and resource records.
type Zone struct {
	Records []Record  `json:"records"`
	SOA     SOARecord `json:"soa"`
}

// SOARecord holds the Start of Authority fields for a zone.
// Serial follows the YYYYMMDDNN convention and is managed by Node.js.
type SOARecord struct {
	Email   string `json:"email"`
	Expire  int    `json:"expire"`
	Minimum int    `json:"minimum"`
	Primary string `json:"primary"`
	Refresh int    `json:"refresh"`
	Retry   int    `json:"retry"`
	Serial  int    `json:"serial"`
	TTL     int    `json:"ttl"`
}

// Record represents a single DNS resource record within a zone.
// The Type field determines which response handler processes this record.
type Record struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Priority int    `json:"priority,omitempty"` // MX priority
	TTL      int    `json:"ttl"`
	Type     string `json:"type"` // A, AAAA, CNAME, MX, TXT, NS, CAA
	Value    string `json:"value"`
}

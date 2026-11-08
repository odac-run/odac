package config

// Config represents the full configuration payload from Node.js
type Config struct {
	Domains  map[string]Website `json:"domains"`
	Firewall Firewall           `json:"firewall"`
	Memory   *Memory            `json:"memory,omitempty"`
	MTLS     *MTLS              `json:"mtls,omitempty"`
	SSL      *SSL               `json:"ssl"`
	Tunnels  []Tunnel           `json:"tunnels"`
}

// MTLS is the client certificate material shared by every record: public CA
// certificates, the host scope of each issued certificate, and revocations.
type MTLS struct {
	CAs     map[string]MTLSCA   `json:"cas"`
	Certs   map[string][]string `json:"certs"`
	Issuers []MTLSIssuer        `json:"issuers,omitempty"`
	Revoked []string            `json:"revoked"`
}

// MTLSIssuer is the server's issuer for one host of a per-host record. Its
// certificate is public: the handshake on that host names it, and a client that
// sends only its leaf is checked against it.
type MTLSIssuer struct {
	CA     string `json:"ca"`
	Cert   string `json:"cert"`
	Direct bool   `json:"direct,omitempty"`
	Host   string `json:"host"`
}

// MTLSCA is one trust anchor. A managed CA issues only certificates ODAC has a
// record of, so its certificates need a scope; an external CA has no records.
type MTLSCA struct {
	Cert    string `json:"cert"`
	Managed bool   `json:"managed"`
}

// WebsiteMTLS is present on every record of an app with a client certificate
// policy, and its presence turns off the parent walk for that record.
type WebsiteMTLS struct {
	CA     []string            `json:"ca,omitempty"`
	Hosts  map[string]HostMTLS `json:"hosts,omitempty"`
	Issuer string              `json:"issuer,omitempty"`
	Key    string              `json:"key"`
	Mode   string              `json:"mode,omitempty"`
}

// HostMTLS overrides a record's policy for one host. An empty CA list inherits
// the record's.
type HostMTLS struct {
	CA   []string `json:"ca,omitempty"`
	Mode string   `json:"mode"`
}

// Memory represents host memory info provided by Node.js (os.totalmem/os.freemem).
// Used by the cache engine to adapt its size to actual available system resources.
type Memory struct {
	Total uint64 `json:"total"` // Total physical RAM in bytes
	Used  uint64 `json:"used"`  // Used RAM in bytes
}

// Tunnel represents a single tunnel endpoint with resolved backend info
type Tunnel struct {
	Domain string `json:"domain"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Token  string `json:"token"`
}

// Website represents a single site configuration
type Website struct {
	Domain      string   `json:"domain"`
	Port        int      `json:"port"`        // The backend port (e.g., 3000, 60001)
	Container   string   `json:"container"`   // Container name (if running in Docker)
	ContainerIP string   `json:"containerIP"` // Direct IP if available
	Subdomains  []string `json:"subdomain"`
	Cert        Cert     `json:"cert"`
	TunnelID    string   `json:"tunnelId,omitempty"` // Non-empty if site is served via remote tunnel

	MTLS *WebsiteMTLS `json:"mtls,omitempty"`
}

// Cert represents SSL certificate paths
type Cert struct {
	SSL SSL `json:"ssl"`
}

// SSL holds key and cert paths
type SSL struct {
	Key  string `json:"key"`
	Cert string `json:"cert"`
}

// Firewall represents firewall rules
type Firewall struct {
	Enabled        bool      `json:"enabled"`
	RateLimit      RateLimit `json:"rateLimit"`
	MaxWSPerIP     int       `json:"maxWsPerIp"`     // Max concurrent WebSockets per IP
	RequestTimeout int       `json:"requestTimeout"` // Timeout for regular HTTP requests in seconds
	Blacklist      []string  `json:"blacklist"`
	Whitelist      []string  `json:"whitelist"`
}

// RateLimit configuration
type RateLimit struct {
	Enabled  bool `json:"enabled"`
	WindowMs int  `json:"windowMs"`
	Max      int  `json:"max"`
}

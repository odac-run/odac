package config

// Config represents the full configuration payload from Node.js
type Config struct {
	Domains  map[string]Website `json:"domains"`
	Firewall Firewall           `json:"firewall"`
	Memory   *Memory            `json:"memory,omitempty"`
	SSL      *SSL               `json:"ssl"`
	Tunnels  []Tunnel           `json:"tunnels"`
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
	Domain      string      `json:"domain"`
	Port        int         `json:"port"`          // The backend port (e.g., 3000, 60001)
	Container   string      `json:"container"`     // Container name (if running in Docker)
	ContainerIP string      `json:"containerIP"`   // Direct IP if available
	Subdomains  []string    `json:"subdomain"`
	Cert        Cert        `json:"cert"`
	TunnelID    string      `json:"tunnelId,omitempty"` // Non-empty if site is served via remote tunnel
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

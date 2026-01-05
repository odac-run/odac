package config

// Config represents the full configuration payload from Node.js
type Config struct {
	Websites map[string]Website `json:"websites"`
	Firewall Firewall           `json:"firewall"`
	SSL      *SSL               `json:"ssl"`
}

// Website represents a single site configuration
type Website struct {
	Domain      string   `json:"domain"`
	Port        int      `json:"port"` // The backend port (e.g., 3000, 60001)
	Pid         interface{} `json:"pid,omitempty"`  // Process ID (string or int)
	Container   string   `json:"container"` // Container name (if running in Docker)
	ContainerIP string   `json:"containerIP"` // Direct IP if available
	Subdomains  []string `json:"subdomain"`
	Cert        Cert     `json:"cert"`
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
	Enabled    bool           `json:"enabled"`
	RateLimit  RateLimit      `json:"rateLimit"`
	MaxWSPerIP int            `json:"maxWsPerIp"` // Max concurrent WebSockets per IP
	Blacklist  []string       `json:"blacklist"`
	Whitelist  []string       `json:"whitelist"`
}

// RateLimit configuration
type RateLimit struct {
	Enabled  bool `json:"enabled"`
	WindowMs int  `json:"windowMs"`
	Max      int  `json:"max"`
}

// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen         string
	DataDir        string
	PublicURL      string
	NodeName       string
	MasterKey      string
	SetupToken     string
	CookieSecure   *bool
	TLSCert        string
	TLSKey         string
	LogLevel       string
	LogFormat      string
	TrustedProxies []netip.Prefix
	MinInterval    time.Duration
	SessionTTL     time.Duration
	SessionIdle    time.Duration
	MetricsPublic  bool
	MetricsToken   string
	AllowedHosts   []string
	ProbeWorkers   int

	// Clustering (Phase 2)
	ClusterListen    string // cluster port (Raft + peer API, mTLS), e.g. ":7443"
	ClusterAdvertise string // host:port peers use to reach this node
	NodeLocation     string // free-form location label (e.g. "fra-1")
	ClusterInit      bool   // found a cluster on first start if not clustered (automation)
	JoinToken        string // join a cluster on first start (automation)
	JoinDataAction   string // "", "export" or "discard" for nodes that already hold data

	// Automation (Phase 3)
	RunnerURL   string // Action Runner used by this node when it is the active controller
	RunnerToken string
}

// secretEnv reads K, or the contents of the file named by K_FILE.
func secretEnv(k string) (string, error) {
	if f := os.Getenv(k + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", k, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(k), nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Listen:     env("INTRABIN_LISTEN", ":8080"),
		DataDir:    env("INTRABIN_DATA_DIR", "./data"),
		PublicURL:  env("INTRABIN_PUBLIC_URL", ""),
		NodeName:   env("INTRABIN_NODE_NAME", ""),
		MasterKey:  os.Getenv("INTRABIN_MASTER_KEY"),
		SetupToken: os.Getenv("INTRABIN_SETUP_TOKEN"),
		TLSCert:    os.Getenv("INTRABIN_TLS_CERT"),
		TLSKey:     os.Getenv("INTRABIN_TLS_KEY"),
		LogLevel:   env("INTRABIN_LOG_LEVEL", "info"),
		LogFormat:  env("INTRABIN_LOG_FORMAT", "json"),

		ClusterListen:    env("INTRABIN_CLUSTER_LISTEN", ":7443"),
		ClusterAdvertise: os.Getenv("INTRABIN_CLUSTER_ADVERTISE"),
		NodeLocation:     os.Getenv("INTRABIN_NODE_LOCATION"),
		JoinToken:        os.Getenv("INTRABIN_JOIN_TOKEN"),
		JoinDataAction:   os.Getenv("INTRABIN_JOIN_DATA_ACTION"),
	}
	if v := os.Getenv("INTRABIN_CLUSTER_INIT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("INTRABIN_CLUSTER_INIT: %w", err)
		}
		c.ClusterInit = b
	}
	c.RunnerURL = os.Getenv("INTRABIN_RUNNER_URL")
	var terr error
	if c.RunnerToken, terr = secretEnv("INTRABIN_RUNNER_TOKEN"); terr != nil {
		return nil, terr
	}
	if c.RunnerURL != "" {
		u, err := url.Parse(c.RunnerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("INTRABIN_RUNNER_URL must be an http(s) URL")
		}
		if len(c.RunnerToken) < 24 {
			return nil, fmt.Errorf("INTRABIN_RUNNER_TOKEN (or INTRABIN_RUNNER_TOKEN_FILE) must be set to at least 24 characters when INTRABIN_RUNNER_URL is set")
		}
	}
	switch c.JoinDataAction {
	case "", "export", "discard":
	default:
		return nil, fmt.Errorf("INTRABIN_JOIN_DATA_ACTION must be export or discard")
	}
	if v := os.Getenv("INTRABIN_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("INTRABIN_COOKIE_SECURE: %w", err)
		}
		c.CookieSecure = &b
	}
	if v := os.Getenv("INTRABIN_TRUSTED_PROXIES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			pref, err := netip.ParsePrefix(p)
			if err != nil {
				addr, err2 := netip.ParseAddr(p)
				if err2 != nil {
					return nil, fmt.Errorf("INTRABIN_TRUSTED_PROXIES: %q: %w", p, err)
				}
				pref = netip.PrefixFrom(addr, addr.BitLen())
			}
			c.TrustedProxies = append(c.TrustedProxies, pref)
		}
	}
	var err error
	if c.MinInterval, err = durEnv("INTRABIN_MIN_INTERVAL", 10*time.Second); err != nil {
		return nil, err
	}
	if c.SessionTTL, err = durEnv("INTRABIN_SESSION_TTL", 7*24*time.Hour); err != nil {
		return nil, err
	}
	if c.SessionIdle, err = durEnv("INTRABIN_SESSION_IDLE", 12*time.Hour); err != nil {
		return nil, err
	}
	c.MetricsPublic, _ = strconv.ParseBool(env("INTRABIN_METRICS_PUBLIC", "false"))
	c.MetricsToken = os.Getenv("INTRABIN_METRICS_TOKEN")
	if v := os.Getenv("INTRABIN_ALLOWED_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
	}
	c.ProbeWorkers, _ = strconv.Atoi(env("INTRABIN_PROBE_WORKERS", "64"))
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("INTRABIN_TLS_CERT and INTRABIN_TLS_KEY must be set together")
	}
	if c.SetupToken != "" && len(c.SetupToken) < 16 {
		return nil, fmt.Errorf("INTRABIN_SETUP_TOKEN must be at least 16 characters")
	}
	return c, nil
}

func durEnv(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

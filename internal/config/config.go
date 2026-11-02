// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net/netip"
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

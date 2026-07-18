// Package config loads wireprompt's optional configuration file. Flags always
// override config values; the zero value is a fully working default setup.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Config is the on-disk configuration at ~/.config/wireprompt/config.json.
type Config struct {
	// Addr is the listen address (default 127.0.0.1:9091).
	Addr string `json:"addr,omitempty"`
	// DB overrides the database path.
	DB string `json:"db,omitempty"`
	// Token gates all HTTP access when binding non-loopback addresses.
	Token string `json:"token,omitempty"`
	// Upstreams maps extra OpenAI-compatible route names to base URLs.
	Upstreams map[string]string `json:"upstreams,omitempty"`
	// Redact holds regex patterns scrubbed from stored bodies.
	Redact []string `json:"redact,omitempty"`
	// NoBodies disables request/response body storage entirely.
	NoBodies bool `json:"no_bodies,omitempty"`
	// InjectUsage controls rewriting streaming OpenAI requests to include
	// usage accounting. Defaults to true when nil.
	InjectUsage *bool `json:"inject_usage,omitempty"`
	// RetentionDays, when > 0, prunes records older than this on startup.
	RetentionDays int `json:"retention_days,omitempty"`

	redactRe []*regexp.Regexp
}

// Path returns the config file location.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "wireprompt", "config.json"), nil
}

// Load reads the config file; a missing file yields the zero config.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return &Config{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if err := c.compile(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) compile() error {
	for _, pat := range c.Redact {
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("redact pattern %q: %w", pat, err)
		}
		c.redactRe = append(c.redactRe, re)
	}
	return nil
}

// SetRedact replaces the redaction patterns (used by tests and flags).
func (c *Config) SetRedact(patterns []string) error {
	c.Redact = patterns
	c.redactRe = nil
	return c.compile()
}

// ApplyRedaction scrubs configured patterns from a body copy. Returns the
// input untouched when no patterns are configured.
func (c *Config) ApplyRedaction(body []byte) []byte {
	if len(c.redactRe) == 0 || len(body) == 0 {
		return body
	}
	out := body
	for _, re := range c.redactRe {
		out = re.ReplaceAll(out, []byte("[REDACTED]"))
	}
	return out
}

// InjectUsageEnabled reports whether streaming usage injection is on.
func (c *Config) InjectUsageEnabled() bool {
	return c.InjectUsage == nil || *c.InjectUsage
}

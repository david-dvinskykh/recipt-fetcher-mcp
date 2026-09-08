// Package config reads the server's settings from the environment.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Config is everything the server needs to start. Credentials are not here:
// they live in the encrypted store under StateDir.
type Config struct {
	// StateDir holds the encrypted credentials and the local key.
	StateDir string
	// LidlCountry and LidlLanguage are used when a Lidl login omits them.
	LidlCountry  string
	LidlLanguage string
	// MailRulesPath overrides the built-in e-mail parsing rules.
	MailRulesPath string
	// UserAgent is sent with every HTTP request.
	UserAgent string
}

// Environment variables read by Load.
const (
	EnvStateDir     = "RECEIPTS_STATE_DIR"
	EnvLidlCountry  = "RECEIPTS_LIDL_COUNTRY"
	EnvLidlLanguage = "RECEIPTS_LIDL_LANGUAGE"
	EnvMailRules    = "RECEIPTS_MAIL_RULES"
	EnvUserAgent    = "RECEIPTS_USER_AGENT"
)

// defaultUserAgent identifies this server honestly rather than impersonating a
// browser; stores that care can tell what is calling them.
const defaultUserAgent = "receipts-mcp/1.0 (+https://github.com/david-dvinskykh/recipt-fetcher-mcp)"

// Load reads the configuration, filling in defaults.
func Load() Config {
	cfg := Config{
		StateDir:      strings.TrimSpace(os.Getenv(EnvStateDir)),
		LidlCountry:   strings.ToUpper(strings.TrimSpace(os.Getenv(EnvLidlCountry))),
		LidlLanguage:  strings.ToLower(strings.TrimSpace(os.Getenv(EnvLidlLanguage))),
		MailRulesPath: strings.TrimSpace(os.Getenv(EnvMailRules)),
		UserAgent:     strings.TrimSpace(os.Getenv(EnvUserAgent)),
	}
	if cfg.StateDir == "" {
		cfg.StateDir = defaultStateDir()
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	return cfg
}

func defaultStateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "receipts-mcp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".receipts-mcp"
	}
	return filepath.Join(home, ".local", "state", "receipts-mcp")
}

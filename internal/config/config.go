// Package config handles the on-disk {name, token, createdAt} state for a
// hearsay instance. First run requires --name and generates a hex bearer
// token; subsequent runs reuse what's stored unless --name or
// --regenerate-token explicitly rotates it.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type Config struct {
	Name      string `json:"name"`
	Token     string `json:"token"`
	CreatedAt string `json:"createdAt"`
}

// Dir returns the hearsay config directory. macOS uses
// ~/Library/Application Support/hearsay; Linux honors $XDG_CONFIG_HOME
// (falling back to ~/.config/hearsay).
func Dir() string {
	return dirFor(runtime.GOOS, os.Getenv("XDG_CONFIG_HOME"))
}

// dirFor is the testable core of Dir — factored out so both platform
// branches are exercisable without cross-compilation tricks.
func dirFor(goos, xdgConfigHome string) string {
	home, _ := os.UserHomeDir()
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "hearsay")
	}
	if xdgConfigHome != "" {
		return filepath.Join(xdgConfigHome, "hearsay")
	}
	return filepath.Join(home, ".config", "hearsay")
}

// Path is the absolute path to config.json.
func Path() string {
	return filepath.Join(Dir(), "config.json")
}

// Load reads config.json. Returns (nil, nil) if the file doesn't exist.
func Load() (*Config, error) {
	raw, err := os.ReadFile(Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config.json is corrupt: %w", err)
	}
	if cfg.Name == "" || cfg.Token == "" {
		return nil, errors.New("config.json missing name or token")
	}
	return &cfg, nil
}

// Save writes config.json with tight permissions. Creates the parent dir
// if needed.
func Save(cfg *Config) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), raw, 0o600)
}

// GenerateToken returns a 32-byte hex-encoded token (64 chars).
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ResolveOptions mirrors the CLI flags that can touch config state.
type ResolveOptions struct {
	NameOverride    string
	RegenerateToken bool
}

// Resolved is the result of Resolve — the effective config plus flags
// the caller uses to decide what to print (first-run token, rotated
// token).
type Resolved struct {
	Config             *Config
	IsFirstRun         bool
	TokenWasRegenerated bool
}

// Resolve loads or creates the config. On first run (no file), --name is
// required; otherwise --name is optional (and overrides the stored name).
func Resolve(opts ResolveOptions) (*Resolved, error) {
	existing, err := Load()
	if err != nil {
		return nil, err
	}

	if existing == nil {
		if opts.NameOverride == "" {
			return nil, fmt.Errorf("first run: --name is required to initialize %s", Path())
		}
		token, err := GenerateToken()
		if err != nil {
			return nil, err
		}
		cfg := &Config{
			Name:      opts.NameOverride,
			Token:     token,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := Save(cfg); err != nil {
			return nil, err
		}
		return &Resolved{Config: cfg, IsFirstRun: true}, nil
	}

	cfg := *existing
	changed := false

	if opts.NameOverride != "" && opts.NameOverride != cfg.Name {
		cfg.Name = opts.NameOverride
		changed = true
	}

	tokenRotated := false
	if opts.RegenerateToken {
		newToken, err := GenerateToken()
		if err != nil {
			return nil, err
		}
		cfg.Token = newToken
		changed = true
		tokenRotated = true
	}

	if changed {
		if err := Save(&cfg); err != nil {
			return nil, err
		}
	}

	return &Resolved{Config: &cfg, TokenWasRegenerated: tokenRotated}, nil
}

// Package config reads and writes ~/.germes/config.yaml. It imports nothing
// from Germes.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole config file.
type Config struct {
	Engines []Engine `yaml:"engines"`
	Blocked []string `yaml:"blocked"`
}

// Engine configures one engine.
type Engine struct {
	Name   string `yaml:"name"`
	Model  string `yaml:"model,omitempty"`  // "" = the engine's default
	Effort string `yaml:"effort,omitempty"` // reasoning effort (codex-api), "" = medium
}

// New returns the default config, written on first run.
func New() Config {
	return Config{
		Engines: []Engine{{Name: "codex-api"}, {Name: "claude-cli"}},
		Blocked: []string{"Bash(sudo *)", "Bash(rm -rf /*)", "Read(.env)", "Read(**/.env*)"},
	}
}

// Load reads the config. A missing file gives the defaults.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.check()
}

// Save writes the config, readable only by its owner.
func Save(path string, c Config) error {
	if err := c.check(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp, path)
}

// GetEngine returns the config of the named engine, or an empty one.
func (c Config) GetEngine(name string) Engine {
	for _, e := range c.Engines {
		if e.Name == name {
			return e
		}
	}
	return Engine{Name: name}
}

func (c Config) check() error {
	if len(c.Engines) == 0 {
		return errors.New("config: no engines")
	}
	for _, e := range c.Engines {
		if e.Name != "claude-cli" && e.Name != "codex-api" {
			return fmt.Errorf("config: unknown engine %q (use codex-api or claude-cli)", e.Name)
		}
	}
	for _, b := range c.Blocked {
		if strings.TrimSpace(b) == "" {
			return errors.New("config: empty entry in blocked")
		}
	}
	return nil
}

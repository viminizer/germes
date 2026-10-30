// Package config reads and writes ~/.germes/config.yaml. It imports nothing
// from Germes.
package config

import (
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/file"
	"github.com/viminizer/germes/internal/tool"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole config file.
type Config struct {
	Engines  []Engine    `yaml:"engines"`
	Blocked  []string    `yaml:"blocked"`
	Hooks    []tool.Hook `yaml:"hooks,omitempty"`     // before a matching call: run a command or load a skill (issue #42); a project adds .germes/hooks.yaml
	CodeDirs []string    `yaml:"code_dirs,omitempty"` // where projects live (Telegram /bind looks here); "" = ~/dev
	// CleanupDays is how long file snapshots and saved tool outputs are
	// kept unused; 0 = 30.
	CleanupDays int      `yaml:"cleanup_days,omitempty"`
	Telegram    Telegram `yaml:"telegram,omitempty"` // empty = off
}

// Telegram is the bot for a group chat with topics. Only UserID in ChatID is
// listened to.
type Telegram struct {
	Token  string `yaml:"token,omitempty"` // from @BotFather
	ChatID int64  `yaml:"chat_id,omitempty"`
	UserID int64  `yaml:"user_id,omitempty"`
	// APIBase is a self-hosted Bot API server; "" = https://api.telegram.org.
	APIBase string `yaml:"api_base,omitempty"`
}

// GetCleanupAge is how long unused snapshots and tool outputs are kept.
func (c Config) GetCleanupAge() time.Duration {
	days := c.CleanupDays
	if days == 0 {
		days = 30
	}
	return time.Duration(days) * 24 * time.Hour
}

// IsOn reports whether the bot is set up.
func (t Telegram) IsOn() bool { return t.Token != "" }

// Engine configures one engine.
type Engine struct {
	Name   string `yaml:"name"`
	Model  string `yaml:"model,omitempty"`  // "" = the engine's default
	Effort string `yaml:"effort,omitempty"` // reasoning effort (codex-api), "" = medium
	// Reserve keeps this much of a usage window free: window -> percent left
	// ("five_hour", "seven_day"). The router stops using the engine there.
	Reserve    map[string]float64 `yaml:"reserve,omitempty"`
	ReserveOff bool               `yaml:"reserve_off,omitempty"` // let it hit the limit
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
	if err := file.Save(path, b, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
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
	for _, e := range c.Engines {
		for w, pct := range e.Reserve {
			if pct < 0 || pct >= 100 {
				return fmt.Errorf("config: engine %s reserve %s: %v%% must be 0 to 99", e.Name, w, pct)
			}
		}
	}
	for _, b := range c.Blocked {
		if strings.TrimSpace(b) == "" {
			return errors.New("config: empty entry in blocked")
		}
	}
	if c.CleanupDays < 0 {
		return errors.New("config: cleanup_days must not be negative")
	}
	if t := c.Telegram; t.IsOn() && (t.ChatID == 0 || t.UserID == 0) {
		return errors.New("config: telegram needs token, chat_id and user_id (run `germes setup telegram`)")
	}
	return nil
}

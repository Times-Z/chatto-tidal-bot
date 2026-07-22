package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds the application configuration loaded from config.json.
type Config struct {
	ChattoURL   string `json:"chatto_url"`
	ChattoToken string `json:"chatto_token"`

	TidalTokenPath string `json:"tidal_token_path"`
	TidalQuality   string `json:"tidal_quality"`

	SampleRate int `json:"sample_rate"`

	LivekitURL string `json:"livekit_url"`

	Rooms []string `json:"rooms"`

	PollInterval Duration `json:"poll_interval"`

	BotName string `json:"bot_name"`

	Volume int `json:"volume"`
}

// Duration is a json-serializable time.Duration that accepts strings
// like "3s", "1m", "500ms" in the config file.
type Duration time.Duration

// UnmarshalJSON parses a duration string (e.g. "3s") into a Duration.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ToDuration converts the custom Duration back to a standard time.Duration.
func (d Duration) ToDuration() time.Duration {
	return time.Duration(d)
}

// LoadConfig reads and parses a JSON config file, applying defaults
// for optional fields and falling back to environment variables.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		PollInterval: Duration(3 * time.Second),
		SampleRate:   48000,
		Volume:       20,
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.ChattoURL == "" {
		cfg.ChattoURL = os.Getenv("CHATTO_URL")
	}
	if cfg.ChattoToken == "" {
		cfg.ChattoToken = os.Getenv("CHATTO_TOKEN")
	}
	if cfg.TidalTokenPath == "" {
		cfg.TidalTokenPath = "tidal_token.json"
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that required configuration is present and coherent.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.ChattoURL) == "" {
		return fmt.Errorf("invalid config: chatto_url is required")
	}
	if err := validateURL(c.ChattoURL, "chatto_url", "http", "https"); err != nil {
		return err
	}

	if strings.TrimSpace(c.ChattoToken) == "" {
		return fmt.Errorf("invalid config: chatto_token is required")
	}

	if strings.TrimSpace(c.LivekitURL) == "" {
		return fmt.Errorf("invalid config: livekit_url is required")
	}
	if err := validateURL(c.LivekitURL, "livekit_url", "ws", "wss"); err != nil {
		return err
	}

	if len(c.Rooms) == 0 {
		return fmt.Errorf("invalid config: rooms must contain at least one room ID")
	}
	for i, roomID := range c.Rooms {
		if strings.TrimSpace(roomID) == "" {
			return fmt.Errorf("invalid config: rooms[%d] must not be empty", i)
		}
	}

	if c.PollInterval.ToDuration() <= 0 {
		return fmt.Errorf("invalid config: poll_interval must be > 0")
	}

	if c.Volume < 0 || c.Volume > 200 {
		return fmt.Errorf("invalid config: volume must be between 0 and 200")
	}

	if c.SampleRate != 44100 && c.SampleRate != 48000 {
		return fmt.Errorf("invalid config: sample_rate must be 44100 or 48000")
	}

	if strings.TrimSpace(c.TidalTokenPath) == "" {
		return fmt.Errorf("invalid config: tidal_token_path must not be empty")
	}

	return nil
}

func validateURL(raw, field string, allowedSchemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid config: %s is not a valid URL: %w", field, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid config: %s must include scheme and host", field)
	}

	for _, s := range allowedSchemes {
		if strings.EqualFold(u.Scheme, s) {
			return nil
		}
	}

	return fmt.Errorf("invalid config: %s must use one of schemes: %s", field, strings.Join(allowedSchemes, ", "))
}

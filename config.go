package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config holds the application configuration loaded from config.json.
type Config struct {
	ChattoURL   string `json:"chatto_url"`
	ChattoToken string `json:"chatto_token"`

	TidalTokenPath string `json:"tidal_token_path"`

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
		Volume:       100,
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

	return cfg, nil
}

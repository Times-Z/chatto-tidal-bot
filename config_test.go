package main

import (
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		ChattoURL:      "https://chat.example.com",
		ChattoToken:    "cht_token",
		TidalTokenPath: "tidal_token.json",
		SampleRate:     48000,
		LivekitURL:     "wss://livekit.example.com",
		Rooms:          []string{"room1"},
		PollInterval:   Duration(3 * time.Second),
		Volume:         20,
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "valid config", mutate: func(c *Config) {}, wantErr: false},
		{name: "missing chatto_url", mutate: func(c *Config) { c.ChattoURL = "" }, wantErr: true},
		{name: "bad chatto_url scheme", mutate: func(c *Config) { c.ChattoURL = "ftp://chat.example.com" }, wantErr: true},
		{name: "missing chatto_token", mutate: func(c *Config) { c.ChattoToken = "" }, wantErr: true},
		{name: "missing livekit_url", mutate: func(c *Config) { c.LivekitURL = "" }, wantErr: true},
		{name: "bad livekit_url scheme", mutate: func(c *Config) { c.LivekitURL = "https://livekit.example.com" }, wantErr: true},
		{name: "no rooms", mutate: func(c *Config) { c.Rooms = nil }, wantErr: true},
		{name: "empty room id", mutate: func(c *Config) { c.Rooms = []string{"room1", "   "} }, wantErr: true},
		{name: "non positive poll interval", mutate: func(c *Config) { c.PollInterval = 0 }, wantErr: true},
		{name: "volume too low", mutate: func(c *Config) { c.Volume = -1 }, wantErr: true},
		{name: "volume too high", mutate: func(c *Config) { c.Volume = 201 }, wantErr: true},
		{name: "unsupported sample rate", mutate: func(c *Config) { c.SampleRate = 96000 }, wantErr: true},
		{name: "empty tidal token path", mutate: func(c *Config) { c.TidalTokenPath = "" }, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no validation error, got: %v", err)
			}
		})
	}
}

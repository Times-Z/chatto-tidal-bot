// Command chatto-tidal-bot is a Chatto music bot that plays Tidal HiFi Plus
// streams in voice channels via LiveKit.
//
// Usage:
//
//	chatto-tidal-bot [config.json]
//
// The config file defaults to "config.json". Tidal authentication happens
// automatically on first launch via device authorization flow.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"chatto-tidal-bot/bot"
	"chatto-tidal-bot/chatto"
)

// main is the application entry point. It loads configuration, creates the bot,
// and runs it until an OS signal (SIGINT/SIGTERM) is received.
func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	configPath := "config.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	chattoClient := chatto.NewClient(cfg.ChattoURL, cfg.ChattoToken)

	botCfg := &bot.Config{
		ChattoURL:      cfg.ChattoURL,
		ChattoToken:    cfg.ChattoToken,
		Rooms:          cfg.Rooms,
		LivekitURL:     cfg.LivekitURL,
		TidalTokenPath: cfg.TidalTokenPath,
		PollInterval:   cfg.PollInterval.ToDuration(),
		BotName:        cfg.BotName,
		Volume:         cfg.Volume,
	}

	b, err := bot.New(ctx, botCfg, chattoClient)
	if err != nil {
		slog.Error("failed to create bot", "error", err)
		os.Exit(1)
	}

	go func() {
		if err := b.Run(ctx); err != nil {
			slog.Error("bot error", "error", err)
			cancel()
		}
	}()

	select {
	case <-sigCh:
		slog.Info("shutting down...")
		cancel()
	case <-ctx.Done():
	}
	b.Shutdown()
}

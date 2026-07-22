//go:build !cgo

package livekit

import (
	"context"
	"errors"
	"io"
	"time"
)

// Player is a no-cgo placeholder used for builds where native audio deps are unavailable.
type Player struct{}

// Config holds connection parameters for creating a new Player.
type Config struct {
	URL        string
	Token      string
	Room       string
	SampleRate int
}

var errCGORequired = errors.New("livekit player requires cgo-enabled build with audio deps")

func NewPlayer(cfg Config) (*Player, error) {
	return nil, errCGORequired
}

func (p *Player) SetVolume(v float64) {}

func (p *Player) Play(ctx context.Context, reader io.ReadCloser, onDone func()) error {
	if reader != nil {
		_ = reader.Close()
	}
	if onDone != nil {
		onDone()
	}
	return errCGORequired
}

func (p *Player) PlaySilenceOnly(ctx context.Context, duration time.Duration) error {
	return errCGORequired
}

func (p *Player) Disconnect() {}

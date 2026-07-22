// Package livekit provides a PCM audio publisher for LiveKit rooms.
// It decodes audio via ffmpeg and publishes PCM16 stereo samples at 48kHz.
package livekit

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os/exec"
	"time"

	"github.com/go-logr/stdr"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	protoLogger "github.com/livekit/protocol/logger"
)

// Player publishes PCM16 stereo audio to a LiveKit room.
// It creates a local audio track, decodes input audio via ffmpeg,
// and writes PCM frames at a real-time pace (20ms per frame).
type Player struct {
	room   *lksdk.Room
	volume float64
}

// SetVolume sets the playback volume multiplier.
// Valid range is 0.0 (silent) to 2.0 (200%). 1.0 is normal volume.
// Changes take effect immediately on subsequent audio frames.
func (p *Player) SetVolume(v float64) {
	if v < 0 {
		v = 0
	}
	if v > 2 {
		v = 2
	}
	p.volume = v
}

// Config holds connection parameters for creating a new Player.
type Config struct {
	URL   string
	Token string
	Room  string
}

// NewPlayer connects to a LiveKit room and returns a Player ready to publish audio.
func NewPlayer(cfg Config) (*Player, error) {
	roomCB := &lksdk.RoomCallback{
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			slog.Warn("livekit disconnected", "reason", reason)
		},
	}

	room, err := lksdk.ConnectToRoomWithToken(cfg.URL, cfg.Token, roomCB,
		lksdk.WithAutoSubscribe(false),
		lksdk.WithDisableTURN(),
		lksdk.WithSinglePeerConnection(),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to livekit room: %w", err)
	}

	slog.Info("connected to livekit room", "room", cfg.Room)
	return &Player{room: room, volume: 1.0}, nil
}

// Play decodes audio from the given reader via ffmpeg and publishes it
// to the LiveKit room as PCM16 stereo at 48kHz.
//
// The reader is closed when Play returns. The onDone callback, if non-nil,
// is called when playback completes or is cancelled.
//
// A silence frame is written every 20ms until the first audio frame arrives
// to prevent the LiveKit track from being dropped.
func (p *Player) Play(ctx context.Context, reader io.ReadCloser, onDone func()) error {
	defer reader.Close()

	logger := protoLogger.LogRLogger(stdr.New(log.Default()))

	track, err := lkmedia.NewPCMLocalTrack(48000, 2, logger)
	if err != nil {
		return fmt.Errorf("create pcm track: %w", err)
	}

	if _, err := p.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "music",
	}); err != nil {
		return fmt.Errorf("publish track: %w", err)
	}

	slog.Info("published track")

	const sampleRate = 48000
	const channels = 2
	const frameDuration = 20 * time.Millisecond
	samplesPerFrame := int(frameDuration.Seconds() * float64(sampleRate))
	frameSize := samplesPerFrame * channels * 2
	silence := make([]int16, samplesPerFrame*channels)

	silenceCtx, silenceStop := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(frameDuration)
		defer ticker.Stop()
		for {
			select {
			case <-silenceCtx.Done():
				return
			case <-ticker.C:
				if err := track.WriteSample(silence); err != nil {
					slog.Warn("silence write sample error", "error", err)
					return
				}
			}
		}
	}()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", "pipe:0",
		"-f", "s16le",
		"-ac", "2",
		"-ar", "48000",
		"-loglevel", "warning",
		"pipe:1",
	)

	cmd.Stdin = reader

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		silenceStop()
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	slog.Info("starting ffmpeg")
	if err := cmd.Start(); err != nil {
		silenceStop()
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	frames := make(chan []int16, 64)

	go func() {
		defer close(frames)
		buf := make([]byte, frameSize)
		var leftover []byte
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				if err == io.EOF && len(leftover) > 0 {
					frames <- bytesToPCM16(leftover)
				}
				return
			}
			data := append(leftover, buf[:n]...)
			for len(data) >= frameSize {
				frames <- bytesToPCM16(data[:frameSize])
				data = data[frameSize:]
			}
			leftover = data
		}
	}()

	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	framesWritten := 0
	gotData := false

	for {
		select {
		case <-ctx.Done():
			slog.Warn("playback context cancelled", "error", ctx.Err())
			cmd.Process.Kill()
			silenceStop()
			track.Close()
			if onDone != nil {
				onDone()
			}
			return ctx.Err()
		case <-ticker.C:
			sample, ok := <-frames
			if !ok {
				silenceStop()
				cmd.Wait()
				if cmd.ProcessState != nil && !cmd.ProcessState.Success() {
					slog.Warn("ffmpeg exit error", "stderr", stderrBuf.String())
				}
				track.Close()
				if onDone != nil {
					onDone()
				}
				slog.Info("playback ended", "frames_written", framesWritten)
				return nil
			}
			if p.volume != 1.0 {
				for i := range sample {
					sample[i] = int16(float64(sample[i]) * p.volume)
				}
			}
			if err := track.WriteSample(sample); err != nil {
				slog.Warn("write sample error", "error", err)
				silenceStop()
				cmd.Process.Kill()
				track.Close()
				if onDone != nil {
					onDone()
				}
				return fmt.Errorf("write sample: %w", err)
			}
			framesWritten++
			if !gotData {
				gotData = true
				slog.Info("first audio frame written to track")
				silenceStop()
			}
		}
	}
}

// PlaySilenceOnly publishes a track and writes silence for the given duration,
// bypassing ffmpeg entirely. Useful for testing LiveKit connectivity.
func (p *Player) PlaySilenceOnly(ctx context.Context, duration time.Duration) error {
	logger := protoLogger.LogRLogger(stdr.New(log.Default()))

	track, err := lkmedia.NewPCMLocalTrack(48000, 2, logger)
	if err != nil {
		return fmt.Errorf("create pcm track: %w", err)
	}

	if _, err := p.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "test-silence",
	}); err != nil {
		return fmt.Errorf("publish track: %w", err)
	}

	slog.Info("silence test: track published")

	silence := make([]int16, 960*2)
	slog.Info("silence test: starting to write silence for", "duration", duration)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(duration)
	for {
		select {
		case <-ctx.Done():
			slog.Warn("silence test: context done", "error", ctx.Err())
			track.Close()
			return ctx.Err()
		case <-timeout:
			slog.Info("silence test: duration elapsed")
			track.Close()
			return nil
		case <-ticker.C:
			if err := track.WriteSample(silence); err != nil {
				slog.Warn("silence test: write sample error", "error", err)
				track.Close()
				return fmt.Errorf("write silence: %w", err)
			}
		}
	}
}

// bytesToPCM16 converts a byte slice of little-endian PCM16 data to int16 samples.
func bytesToPCM16(data []byte) []int16 {
	if len(data)%2 != 0 {
		data = data[:len(data)-1]
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return samples
}

// Disconnect closes the LiveKit room connection.
func (p *Player) Disconnect() {
	if p.room != nil {
		p.room.Disconnect()
	}
}

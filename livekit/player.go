package livekit

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/go-logr/stdr"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	protoLogger "github.com/livekit/protocol/logger"
)

type Player struct {
	room *lksdk.Room
}

type Config struct {
	URL   string
	Token string
	Room  string
}

func NewPlayer(cfg Config) (*Player, error) {
	roomCB := &lksdk.RoomCallback{}

	room, err := lksdk.ConnectToRoomWithToken(cfg.URL, cfg.Token, roomCB)
	if err != nil {
		return nil, fmt.Errorf("connect to livekit room: %w", err)
	}

	return &Player{room: room}, nil
}

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

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", "pipe:0",
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"-ar", "48000",
		"-ac", "2",
		"-loglevel", "error",
		"pipe:1",
	)
	cmd.Stdin = reader

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}
	go io.Copy(os.Stderr, stderr)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	const sampleRate = 48000
	const channels = 2
	const frameDuration = 20 * time.Millisecond
	samplesPerFrame := int(frameDuration.Seconds() * float64(sampleRate))
	frameSize := samplesPerFrame * channels * 2

	buf := make([]byte, frameSize)
	var leftover []byte

	for {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
			track.Close()
			if onDone != nil {
				onDone()
			}
			return ctx.Err()
		default:
		}

		n, err := stdout.Read(buf)
		if err != nil {
			if err == io.EOF && len(leftover) > 0 {
				sample := bytesToPCM16(leftover)
				track.WriteSample(sample)
			}
			cmd.Wait()
			track.Close()
			if onDone != nil {
				onDone()
			}
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read ffmpeg output: %w", err)
		}

		if n > 0 {
			data := append(leftover, buf[:n]...)
			for len(data) >= frameSize {
				sample := bytesToPCM16(data[:frameSize])
				if err := track.WriteSample(sample); err != nil {
					log.Printf("write sample error: %v", err)
				}
				data = data[frameSize:]
			}
			leftover = data
		}
	}
}

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

func (p *Player) Disconnect() {
	if p.room != nil {
		p.room.Disconnect()
	}
}

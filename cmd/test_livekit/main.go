package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/go-logr/stdr"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	lksdk.SetLogger(protoLogger.LogRLogger(stdr.New(slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug))))

	token := os.Getenv("LIVEKIT_TOKEN")
	url := os.Getenv("LIVEKIT_URL")
	if url == "" {
		url = "wss://livekit.mokiki.fr"
	}
	if token == "" {
		slog.Error("LIVEKIT_TOKEN env var required")
		os.Exit(1)
	}

	roomCB := &lksdk.RoomCallback{
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			slog.Warn(">>>> DISCONNECTED", "reason", reason)
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				slog.Info("track subscribed", "kind", track.Kind())
			},
		},
	}

	// Try with and without options on alternating test runs
	useOptions := len(os.Args) > 1 && os.Args[1] == "--basic"
	var room *lksdk.Room
	var err error
	if useOptions {
		slog.Info("connecting with basic options")
		room, err = lksdk.ConnectToRoomWithToken(url, token, roomCB)
	} else {
		slog.Info("connecting with all options")
		room, err = lksdk.ConnectToRoomWithToken(url, token, roomCB,
			lksdk.WithAutoSubscribe(false),
			lksdk.WithDisableTURN(),
			lksdk.WithSinglePeerConnection(),
		)
	}
	if err != nil {
		slog.Error("connect failed", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to room", "room", room.Name())

	// Wait a bit then try publishing
	time.Sleep(2 * time.Second)

	slog.Info("creating PCM track")
	logger := protoLogger.LogRLogger(stdr.New(slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug)))
	track, err := lkmedia.NewPCMLocalTrack(48000, 2, logger)
	if err != nil {
		slog.Error("create track failed", "error", err)
		os.Exit(1)
	}

	slog.Info("publishing track...")
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "test-audio",
	})
	if err != nil {
		slog.Error("publish track failed", "error", err)
		os.Exit(1)
	}
	slog.Info("track published", "sid", pub.SID())

	// Write silence for 10 seconds
	slog.Info("writing silence for 10s")
	silence := make([]int16, 960*2) // 20ms at 48kHz stereo
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 500; i++ {
			err := track.WriteSample(silence)
			if err != nil {
				slog.Warn("write sample error at iteration", "i", i, "error", err)
				done <- struct{}{}
				return
			}
			<-ticker.C
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
		slog.Info("silence writing complete")
	case <-time.After(12 * time.Second):
		slog.Info("timeout waiting for silence")
	}

	slog.Info("disconnecting")
	_ = track.Close()
	room.Disconnect()
	slog.Info("done")
	time.Sleep(1 * time.Second)
}

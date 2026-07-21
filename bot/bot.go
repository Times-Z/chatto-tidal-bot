package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"chatto-tidal-bot/chatto"
	"chatto-tidal-bot/livekit"
	"chatto-tidal-bot/tidal"
)

const errNotMember = "not a member of this room"
const errPermissionDenied = "permission denied"

type Bot struct {
	cfg          *Config
	chattoClient *chatto.Client
	tidalClient  *tidal.Client

	queue *Queue

	mu            sync.Mutex
	currentRoom   string
	activePlayer  *livekit.Player
	running       bool
	playCtx       context.Context
	playCancel    context.CancelFunc
}

type Config struct {
	ChattoURL     string
	ChattoToken   string
	Rooms         []string
	LivekitURL    string
	TidalTokenPath string
	PollInterval  time.Duration
}

func New(ctx context.Context, cfg *Config, chattoClient *chatto.Client) (*Bot, error) {
	tidalClient, err := tidal.NewClient(ctx, cfg.TidalTokenPath)
	if err != nil {
		return nil, fmt.Errorf("create tidal client: %w", err)
	}

	return &Bot{
		cfg:          cfg,
		chattoClient: chattoClient,
		tidalClient:  tidalClient,
		queue:        NewQueue(),
	}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	slog.Info("bot started",
		"rooms", b.cfg.Rooms,
		"poll_interval", b.cfg.PollInterval,
	)

	for _, roomID := range b.cfg.Rooms {
		b.chattoClient.CreateMessage(ctx, roomID, "🤖 Tidal Bot ready! Commands: `/play <query>`, `/queue <query>`, `/skip`, `/stop`, `/nowplaying`, `/help`")
	}

	cursors := make(map[string]string)

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, roomID := range b.cfg.Rooms {
				after := cursors[roomID]
				resp, err := b.chattoClient.GetRoomEvents(ctx, roomID, after, 50)
				if err != nil {
					if strings.Contains(err.Error(), errNotMember) || strings.Contains(err.Error(), errPermissionDenied) {
						slog.Info("not a member, trying to self-add to room", "room", roomID)
						userID, viewErr := b.chattoClient.GetViewer(ctx)
						if viewErr != nil {
							slog.Error("get viewer failed", "error", viewErr)
							continue
						}
						slog.Info("got bot user ID, adding to room", "user_id", userID, "room", roomID)
						if addErr := b.chattoClient.AddMember(ctx, roomID, userID); addErr != nil {
							slog.Error("add member failed", "room", roomID, "error", addErr)
							continue
						}
						// Retry immediately
						resp, err = b.chattoClient.GetRoomEvents(ctx, roomID, after, 50)
						if err != nil {
							slog.Error("poll room events after join", "room", roomID, "error", err)
							continue
						}
					} else {
						slog.Error("poll room events", "room", roomID, "error", err)
						continue
					}
				}
				if resp == nil || resp.Page == nil {
					continue
				}
				page := resp.Page
				if len(page.Events) > 0 {
					cursors[roomID] = page.EndCursor
				}
				for _, event := range page.Events {
					if event.Event.MessagePosted == nil {
						continue
					}
					msg := event.Event.MessagePosted.Message
					if msg.Body == nil || *msg.Body == "" {
						continue
					}
					b.handleMessage(ctx, roomID, msg)
				}
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, roomID string, msg chatto.Message) {
	parsed := parseCommand(*msg.Body)
	if parsed == nil {
		return
	}

	slog.Info("command",
		"cmd", parsed.Command,
		"args", parsed.Args,
		"user", msg.ActorID,
		"room", roomID,
	)

	switch parsed.Command {
	case CmdHelp:
		b.sendMessage(ctx, roomID, "Commands:\n`/play <query>` — Search and play a track\n`/queue <query>` — Add to queue\n`/skip` — Skip current track\n`/stop` — Stop and clear queue\n`/nowplaying` — Show current track\n`/help` — Show this help")

	case CmdPlay:
		b.cmdPlay(ctx, roomID, msg.ActorID, parsed.Args)

	case CmdQueue:
		b.cmdQueue(ctx, roomID, msg.ActorID, parsed.Args)

	case CmdSkip:
		b.cmdSkip(ctx, roomID)

	case CmdStop:
		b.cmdStop(ctx, roomID)

	case CmdNowPlaying:
		b.cmdNowPlaying(ctx, roomID)
	}
}

func (b *Bot) cmdPlay(ctx context.Context, roomID, actorID, query string) {
	if query == "" {
		b.sendMessage(ctx, roomID, "Usage: `/play <track name>` — e.g. `/play Daft Punk Around the World`")
		return
	}

	results, err := b.tidalClient.Search(ctx, query, 5)
	if err != nil {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Search error: %v", err))
		return
	}

	if len(results) == 0 {
		b.sendMessage(ctx, roomID, fmt.Sprintf("No results for: %s", query))
		return
	}

	track := results[0]

	if len(results) > 1 {
		var msg strings.Builder
		msg.WriteString(fmt.Sprintf("Top results for \"%s\":\n", query))
		for i, r := range results {
			msg.WriteString(fmt.Sprintf("`%d.` %s — %s\n", i+1, r.Artist, r.Title))
		}
		msg.WriteString(fmt.Sprintf("\nPlaying first result: **%s — %s**", track.Artist, track.Title))
		b.sendMessage(ctx, roomID, msg.String())
	}

	b.queue.Add(Track{
		TID:       track.ID,
		Title:     track.Title,
		Artist:    track.Artist,
		Duration:  track.Duration,
		Requestor: actorID,
	})

	b.sendMessage(ctx, roomID, fmt.Sprintf("🎵 Added to queue: **%s — %s**", track.Artist, track.Title))

	b.startPlayback(ctx, roomID)
}

func (b *Bot) cmdQueue(ctx context.Context, roomID, actorID, query string) {
	if query == "" {
		tracks := b.queue.List()
		if b.queue.Len() == 0 {
			b.sendMessage(ctx, roomID, "Queue is empty.")
			return
		}
		var msg strings.Builder
		msg.WriteString(fmt.Sprintf("Queue (%d):\n", len(tracks)))
		for i, t := range tracks {
			msg.WriteString(fmt.Sprintf("`%d.` %s — %s\n", i+1, t.Artist, t.Title))
		}
		b.sendMessage(ctx, roomID, msg.String())
		return
	}

	results, err := b.tidalClient.Search(ctx, query, 3)
	if err != nil {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Search error: %v", err))
		return
	}
	if len(results) == 0 {
		b.sendMessage(ctx, roomID, fmt.Sprintf("No results for: %s", query))
		return
	}

	track := results[0]
	b.queue.Add(Track{
		TID:       track.ID,
		Title:     track.Title,
		Artist:    track.Artist,
		Duration:  track.Duration,
		Requestor: actorID,
	})
	b.sendMessage(ctx, roomID, fmt.Sprintf("🎵 Added to queue: **%s — %s**", track.Artist, track.Title))
}

func (b *Bot) cmdSkip(ctx context.Context, roomID string) {
	b.mu.Lock()
	if b.playCancel != nil {
		b.playCancel()
	}
	b.mu.Unlock()
	b.sendMessage(ctx, roomID, "⏭ Skipped")
}

func (b *Bot) cmdStop(ctx context.Context, roomID string) {
	b.mu.Lock()
	if b.playCancel != nil {
		b.playCancel()
	}
	b.mu.Unlock()
	b.queue.Clear()
	b.sendMessage(ctx, roomID, "⏹ Stopped and queue cleared")
}

func (b *Bot) cmdNowPlaying(ctx context.Context, roomID string) {
	track, ok := b.queue.Current()
	if !ok {
		b.sendMessage(ctx, roomID, "Nothing playing right now.")
		return
	}
	b.sendMessage(ctx, roomID, fmt.Sprintf("🎵 Now playing: **%s — %s**", track.Artist, track.Title))
}

func (b *Bot) startPlayback(ctx context.Context, roomID string) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.currentRoom = roomID
	b.mu.Unlock()

	go func() {
		for {
			track, ok := b.queue.Next()
			if !ok {
				b.endPlayback(roomID)
				return
			}

			playCtx, cancel := context.WithCancel(context.Background())
			b.mu.Lock()
			b.playCtx = playCtx
			b.playCancel = cancel
			b.mu.Unlock()

			err := b.playTrack(playCtx, roomID, track)
			cancel()

			if err != nil {
				slog.Error("play track error", "error", err)
				b.chattoClient.CreateMessage(context.Background(), roomID, fmt.Sprintf("Playback error: %v", err))
			}
		}
	}()
}

func (b *Bot) endPlayback(roomID string) {
	b.mu.Lock()
	b.running = false
	b.currentRoom = ""
	b.mu.Unlock()

	b.chattoClient.CreateMessage(context.Background(), roomID, "Queue empty. Add more songs with `/play` or `/queue`.")
	b.leaveCall(context.Background(), roomID)
}

func (b *Bot) playTrack(ctx context.Context, roomID string, track Track) error {
	b.chattoClient.CreateMessage(context.Background(), roomID,
		fmt.Sprintf("🎵 Now playing: **%s — %s** (requested by %s)", track.Artist, track.Title, track.Requestor))

	joined, err := b.chattoClient.JoinCall(ctx, roomID)
	if err != nil {
		return fmt.Errorf("join call: %w", err)
	}
	if !joined {
		b.chattoClient.CreateMessage(context.Background(), roomID, "⚠ No active voice call in this room. Start one first!")
		return nil
	}

	token, err := b.chattoClient.GetCallToken(ctx, roomID)
	if err != nil {
		return fmt.Errorf("get call token: %w", err)
	}

	player, err := livekit.NewPlayer(livekit.Config{
		URL:   b.cfg.LivekitURL,
		Token: token.Token,
		Room:  roomID,
	})
	if err != nil {
		return fmt.Errorf("create livekit player: %w", err)
	}

	b.mu.Lock()
	b.activePlayer = player
	b.mu.Unlock()

	defer func() {
		player.Disconnect()
		b.mu.Lock()
		b.activePlayer = nil
		b.mu.Unlock()
	}()

	stream, err := b.tidalClient.StreamTrack(ctx, track.TID)
	if err != nil {
		return fmt.Errorf("stream track: %w", err)
	}
	defer stream.Reader.Close()

	return player.Play(ctx, stream.Reader, nil)
}

func (b *Bot) leaveCall(ctx context.Context, roomID string) {
	if _, err := b.chattoClient.LeaveCall(ctx, roomID); err != nil {
		slog.Error("leave call", "error", err)
	}
}

func (b *Bot) sendMessage(ctx context.Context, roomID, text string) {
	if err := b.chattoClient.CreateMessage(ctx, roomID, text); err != nil {
		slog.Error("send message", "error", err)
	}
}

func (b *Bot) Shutdown() {
	slog.Info("shutting down bot...")
	b.mu.Lock()
	if b.playCancel != nil {
		b.playCancel()
	}
	if b.activePlayer != nil {
		b.activePlayer.Disconnect()
	}
	b.mu.Unlock()
	b.tidalClient.Close()
}



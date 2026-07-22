// Package bot implements the Chatto Tidal Bot — a music bot that plays
// Tidal HiFi Plus streams in Chatto voice channels via LiveKit.
//
// The bot polls Chatto room events for commands, resolves tracks via Tidal
// API (search or URL), queues them, and publishes PCM16 audio to LiveKit.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatto-tidal-bot/chatto"
	"chatto-tidal-bot/livekit"
	"chatto-tidal-bot/tidal"
)

const errNotMember = "not a member of this room"
const errPermissionDenied = "permission denied"

// artistSuffix returns " by Artist" if artist is non-empty, otherwise "".
func artistSuffix(artist string) string {
	if artist == "" {
		return ""
	}
	return " by " + artist
}

// formatDuration formats seconds as "m:ss".
func formatDuration(seconds int) string {
	m := seconds / 60
	s := seconds % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatTrack returns a Markdown-formatted track string with title and duration.
func formatTrack(t Track) string {
	return fmt.Sprintf("**%s** — `%s`", t.Title, formatDuration(t.Duration))
}

// queueETA returns an estimated-time-until-play string based on total
// remaining duration, or empty string if fewer than 2 tracks remain.
func (b *Bot) queueETA() string {
	if b.queue.Len() <= 1 {
		return ""
	}
	total := 0
	for _, t := range b.queue.List() {
		total += t.Duration
	}
	return fmt.Sprintf("· ~%s until play", formatDuration(total))
}

// Bot is the main bot instance. It manages the queue, processes chat
// commands, and orchestrates Tidal streaming and LiveKit playback.
type Bot struct {
	cfg          *Config
	chattoClient *chatto.Client
	tidalClient  *tidal.Client
	botName      string
	botUserID    string
	startedAt    time.Time

	queue *Queue

	mu             sync.Mutex
	currentRoom    string
	activePlayer   *livekit.Player
	running        bool
	volume         float64
	playCtx        context.Context
	playCancel     context.CancelFunc
	trackStartTime time.Time
	trackAudioInfo string

	seenEvents   map[string]struct{}
	seenEventsMu sync.Mutex
}

// Config holds the bot's runtime configuration.
type Config struct {
	ChattoURL      string
	ChattoToken    string
	Rooms          []string
	LivekitURL     string
	TidalTokenPath string
	PollInterval   time.Duration
	BotName        string
	Volume         int
}

// New creates a new Bot, initializing the Tidal client and fetching
// the bot's own user ID for message filtering.
func New(ctx context.Context, cfg *Config, chattoClient *chatto.Client) (*Bot, error) {
	tidalClient, err := tidal.NewClient(ctx, cfg.TidalTokenPath)
	if err != nil {
		return nil, fmt.Errorf("create tidal client: %w", err)
	}

	botName := cfg.BotName
	if botName == "" {
		botName = "tidal.bot"
	}

	botUserID, _ := chattoClient.GetViewer(ctx)
	if botUserID == "" {
		slog.Warn("could not get bot user ID, own messages will not be filtered")
	}

	return &Bot{
		cfg:          cfg,
		chattoClient: chattoClient,
		tidalClient:  tidalClient,
		botName:      botName,
		botUserID:    botUserID,
		startedAt:    time.Now(),
		queue:        NewQueue(),
		volume:       float64(cfg.Volume) / 100.0,
		seenEvents:   make(map[string]struct{}),
	}, nil
}

// Run enters the main event loop, polling all configured rooms for new
// events at the configured interval. It handles messages, auto-joins
// rooms as needed, and auto-starts playback when tracks are queued.
func (b *Bot) Run(ctx context.Context) error {
	slog.Info("bot started",
		"rooms", b.cfg.Rooms,
		"poll_interval", b.cfg.PollInterval,
	)

	b.setPresence(ctx)

	cursors := make(map[string]string)

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	pollCount := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pollCount++
			if pollCount%100 == 0 {
				b.resetSeenEvents()
			}

			for _, roomID := range b.cfg.Rooms {
				b.pollRoom(ctx, roomID, cursors)
			}
		}

		b.autoStartPlayback(ctx)
	}
}

// setPresence sets the bot's online status and custom status on Chatto.
func (b *Bot) setPresence(ctx context.Context) {
	if err := b.chattoClient.UpdatePresence(ctx, "PRESENCE_STATUS_ONLINE", true); err != nil {
		slog.Warn("set online presence", "error", err)
	}
	if err := b.chattoClient.UpdateCustomStatus(ctx, "🎧", "Providing high-res songs"); err != nil {
		slog.Warn("set custom status", "error", err)
	}
}

// resetSeenEvents clears the deduplication set to prevent unbounded memory growth.
func (b *Bot) resetSeenEvents() {
	b.seenEventsMu.Lock()
	b.seenEvents = make(map[string]struct{})
	b.seenEventsMu.Unlock()
}

// pollRoom fetches and processes new events for a single room.
func (b *Bot) pollRoom(ctx context.Context, roomID string, cursors map[string]string) {
	after := cursors[roomID]
	slog.Debug("polling room events", "room", roomID, "cursor", after)
	resp, err := b.chattoClient.GetRoomEvents(ctx, roomID, after, 50)
	if err != nil {
		b.handlePollError(ctx, roomID, err, after)
		return
	}
	if resp == nil || resp.Page == nil {
		return
	}
	page := resp.Page
	cursors[roomID] = page.EndCursor
	if len(page.Events) > 0 {
		slog.Info("received events", "room", roomID, "count", len(page.Events), "cursor", page.EndCursor)
	} else {
		slog.Debug("no new events", "room", roomID, "cursor", page.EndCursor)
	}
	for _, event := range page.Events {
		b.processEvent(ctx, roomID, event)
	}
}

// handlePollError handles errors from GetRoomEvents, attempting to
// self-add the bot to the room if it's not a member.
func (b *Bot) handlePollError(ctx context.Context, roomID string, err error, after string) {
	if strings.Contains(err.Error(), errNotMember) || strings.Contains(err.Error(), errPermissionDenied) {
		slog.Info("not a member, trying to self-add to room", "room", roomID)
		userID, viewErr := b.chattoClient.GetViewer(ctx)
		if viewErr != nil {
			slog.Error("get viewer failed", "error", viewErr)
			return
		}
		slog.Info("got bot user ID, adding to room", "user_id", userID, "room", roomID)
		if addErr := b.chattoClient.AddMember(ctx, roomID, userID); addErr != nil {
			slog.Error("add member failed", "room", roomID, "error", addErr)
			return
		}
		resp, err := b.chattoClient.GetRoomEvents(ctx, roomID, after, 50)
		if err != nil {
			slog.Error("poll room events after join", "room", roomID, "error", err)
		}
		_ = resp
	} else {
		slog.Error("poll room events", "room", roomID, "error", err)
	}
}

// processEvent handles a single room timeline event, filtering out
// events that should be skipped (own messages, before-bot-start, etc.)
// and dispatching message events to handleMessage.
func (b *Bot) processEvent(ctx context.Context, roomID string, event chatto.RoomTimelineEvent) {
	if event.MessagePosted == nil {
		return
	}
	msg := event.MessagePosted.Message
	if msg.Body == nil || *msg.Body == "" {
		return
	}

	b.seenEventsMu.Lock()
	_, seen := b.seenEvents[event.ID]
	if !seen {
		b.seenEvents[event.ID] = struct{}{}
	}
	b.seenEventsMu.Unlock()
	if seen {
		slog.Debug("skipping already processed event", "room", roomID, "event_id", event.ID)
		return
	}

	if msg.ActorID == b.botUserID {
		slog.Debug("skipping own message", "room", roomID, "event_id", event.ID)
		return
	}

	if parseEventTime(event.CreatedAt).Before(b.startedAt) {
		slog.Debug("skipping event before bot start", "room", roomID, "event_id", event.ID, "created_at", event.CreatedAt)
		return
	}

	slog.Info("received message", "room", roomID, "body", *msg.Body, "actor", msg.ActorID)
	b.handleMessage(ctx, roomID, msg)
}

// autoStartPlayback starts playback in any room that has queued tracks
// but is not currently playing. Idempotent — only starts if not running.
func (b *Bot) autoStartPlayback(ctx context.Context) {
	for _, roomID := range b.cfg.Rooms {
		b.mu.Lock()
		running := b.running
		b.mu.Unlock()
		if !running && b.queue.Len() > 0 {
			b.startPlayback(ctx, roomID)
		}
	}
}

// handleMessage parses a chat message as a bot command and dispatches
// to the appropriate command handler.
func (b *Bot) handleMessage(ctx context.Context, roomID string, msg chatto.Message) {
	parsed := parseCommand(*msg.Body, b.botName)
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
		b.cmdHelp(ctx, roomID)

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

	case CmdVolume:
		b.cmdVolume(ctx, roomID, parsed.Args)

	case CmdTest:
		b.cmdTest(ctx, roomID)
	}
}

func (b *Bot) cmdHelp(ctx context.Context, roomID string) {
	b.sendMessage(ctx, roomID, "🎧 **Tidal Bot** — Commands\n\n`play <track>` — Play a track\n`queue <track>` — Add to queue\n`queue` — View queue\n`skip` — Skip to next\n`stop` — Stop & clear queue\n`nowplaying` — Now playing\n`volume <0-200>` — Set volume\n`help` — This message")
}

// cmdTest tests LiveKit connectivity by publishing 10 seconds of silence.
func (b *Bot) cmdTest(ctx context.Context, roomID string) {
	b.sendMessage(ctx, roomID, "🔇 Testing LiveKit connection...")

	go func() {
		joined, err := b.chattoClient.JoinCall(ctx, roomID)
		if err != nil {
			b.sendMessage(ctx, roomID, fmt.Sprintf("Join call error: %v", err))
			return
		}
		if !joined {
			b.sendMessage(ctx, roomID, "Join a voice channel first.")
			return
		}

		token, err := b.chattoClient.GetCallToken(ctx, roomID)
		if err != nil {
			b.sendMessage(ctx, roomID, fmt.Sprintf("Token error: %v", err))
			return
		}

		player, err := livekit.NewPlayer(livekit.Config{
			URL:   b.cfg.LivekitURL,
			Token: token.Token,
			Room:  roomID,
		})
		if err != nil {
			b.sendMessage(ctx, roomID, fmt.Sprintf("Player error: %v", err))
			return
		}

		err = player.PlaySilenceOnly(ctx, 10*time.Second)
		player.Disconnect()

		if err != nil {
			b.sendMessage(ctx, roomID, fmt.Sprintf("Test failed: %v", err))
		} else {
			b.sendMessage(ctx, roomID, "✅ LiveKit connection OK · 10s silence")
		}
	}()
}

// resolveTracks resolves a user query into track results. It first tries
// to parse the query as a Tidal URL (track/album/playlist/artist), falling
// back to text search.
func (b *Bot) resolveTracks(ctx context.Context, query string) ([]tidal.SearchResult, string, error) {
	if contentType, id, err := tidal.ParseTidalURL(query); err == nil {
		slog.Info("detected tidal URL", "type", contentType, "id", id)
		switch contentType {
		case "track":
			return b.resolveTrackURL(ctx, id)
		case "album":
			return b.resolveAlbumURL(ctx, id)
		case "playlist":
			return b.resolvePlaylistURL(ctx, id)
		case "artist":
			return b.resolveArtistURL(ctx, id)
		}
	} else {
		slog.Warn("not a tidal URL", "error", err, "query", query)
	}
	results, err := b.tidalClient.Search(ctx, query, 5)
	if err != nil {
		return nil, "", err
	}
	if len(results) == 0 {
		return nil, "", nil
	}
	return results, "search", nil
}

func (b *Bot) resolveTrackURL(ctx context.Context, id string) ([]tidal.SearchResult, string, error) {
	tid, parseErr := strconv.ParseUint(id, 10, 64)
	if parseErr != nil {
		return nil, "", fmt.Errorf("invalid track ID: %s", id)
	}
	track, err := b.tidalClient.GetTrack(ctx, tid)
	if err != nil {
		return nil, "", fmt.Errorf("get track: %w", err)
	}
	return []tidal.SearchResult{{ID: track.ID, Title: track.Title, Artist: track.Artist, Duration: track.Duration}}, "track", nil
}

func (b *Bot) resolveAlbumURL(ctx context.Context, id string) ([]tidal.SearchResult, string, error) {
	aid, parseErr := strconv.ParseUint(id, 10, 64)
	if parseErr != nil {
		return nil, "", fmt.Errorf("invalid album ID: %s", id)
	}
	tracks, err := b.tidalClient.GetAlbumTracks(ctx, aid)
	if err != nil {
		return nil, "", fmt.Errorf("get album: %w", err)
	}
	return tracks, "album", nil
}

func (b *Bot) resolvePlaylistURL(ctx context.Context, id string) ([]tidal.SearchResult, string, error) {
	tracks, err := b.tidalClient.GetPlaylistTracks(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("get playlist: %w", err)
	}
	return tracks, "playlist", nil
}

func (b *Bot) resolveArtistURL(ctx context.Context, id string) ([]tidal.SearchResult, string, error) {
	aid, parseErr := strconv.ParseUint(id, 10, 64)
	if parseErr != nil {
		return nil, "", fmt.Errorf("invalid artist ID: %s", id)
	}
	tracks, err := b.tidalClient.GetArtistTopTracks(ctx, aid)
	if err != nil {
		return nil, "", fmt.Errorf("get artist tracks: %w", err)
	}
	return tracks, "artist", nil
}

// cmdPlay searches for or resolves a track, queues it, and starts playback.
func (b *Bot) cmdPlay(ctx context.Context, roomID, actorID, query string) {
	if query == "" {
		b.sendMessage(ctx, roomID, "Usage: `/play <track name>` — e.g. `/play Daft Punk Around the World`")
		return
	}

	tracks, sourceType, err := b.resolveTracks(ctx, query)
	if err != nil {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(tracks) == 0 {
		b.sendMessage(ctx, roomID, fmt.Sprintf("No results for: %s", query))
		return
	}

	if sourceType == "search" {
		if len(tracks) > 1 {
			var msg strings.Builder
			msg.WriteString(fmt.Sprintf("🔍 Top results for \"**%s**\":\n", query))
			for i, r := range tracks {
				msg.WriteString(fmt.Sprintf("`%d.` %s\n", i+1, formatTrack(Track{Title: r.Title, Artist: r.Artist, Duration: r.Duration})))
			}
			msg.WriteString(fmt.Sprintf("\nPlaying first result: %s", formatTrack(Track{Title: tracks[0].Title, Artist: tracks[0].Artist, Duration: tracks[0].Duration})))
			b.sendMessage(ctx, roomID, msg.String())
		}
		tracks = tracks[:1]
	} else {
		sourceLabel := map[string]string{"track": "Track", "album": "Album", "playlist": "Playlist", "artist": "Artist Top Tracks"}[sourceType]
		b.sendMessage(ctx, roomID, fmt.Sprintf("📦 Loading **%s** (%d track(s))...", sourceLabel, len(tracks)))
	}

	b.addTracksToQueue(ctx, roomID, actorID, tracks)
}

// cmdQueue adds a track to the queue without starting playback, or lists
// the current queue if no query is given.
func (b *Bot) cmdQueue(ctx context.Context, roomID, actorID, query string) {
	if query == "" {
		b.printQueue(ctx, roomID)
		return
	}

	tracks, sourceType, err := b.resolveTracks(ctx, query)
	if err != nil {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(tracks) == 0 {
		b.sendMessage(ctx, roomID, fmt.Sprintf("No results for: %s", query))
		return
	}

	if sourceType == "search" {
		tracks = tracks[:1]
	} else {
		sourceLabel := map[string]string{"track": "Track", "album": "Album", "playlist": "Playlist", "artist": "Artist Top Tracks"}[sourceType]
		b.sendMessage(ctx, roomID, fmt.Sprintf("📦 Loading **%s** (%d track(s))...", sourceLabel, len(tracks)))
	}

	b.addTracksToQueue(ctx, roomID, actorID, tracks)
}

// printQueue sends a formatted message listing all tracks in the queue.
func (b *Bot) printQueue(ctx context.Context, roomID string) {
	tracks := b.queue.List()
	if b.queue.Len() == 0 {
		b.sendMessage(ctx, roomID, "📋 Queue is empty.")
		return
	}
	total := 0
	for _, t := range tracks {
		total += t.Duration
	}
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("📋 **Queue** · %d tracks · `%s`\n", len(tracks), formatDuration(total)))
	for i, t := range tracks {
		msg.WriteString(fmt.Sprintf("`%d.` %s\n", i+1, formatTrack(t)))
	}
	b.sendMessage(ctx, roomID, msg.String())
}

// addTracksToQueue adds resolved tracks to the queue and sends a
// confirmation message with queue position and ETA.
func (b *Bot) addTracksToQueue(ctx context.Context, roomID, actorID string, tracks []tidal.SearchResult) {
	for _, r := range tracks {
		b.queue.Add(Track{
			TID:       r.ID,
			Title:     r.Title,
			Artist:    r.Artist,
			Duration:  r.Duration,
			Requestor: actorID,
		})
	}

	if len(tracks) == 1 {
		r := tracks[0]
		pos := b.queue.Len()
		msg := fmt.Sprintf("🎵 Added #**%d** — %s", pos, formatTrack(Track{Title: r.Title, Artist: r.Artist, Duration: r.Duration}))
		if eta := b.queueETA(); eta != "" {
			msg += " " + eta
		}
		b.sendMessage(ctx, roomID, msg)
	} else {
		b.sendMessage(ctx, roomID, fmt.Sprintf("🎵 Added **%d** tracks to queue", len(tracks)))
	}
}

// cmdSkip cancels the current playback, advancing to the next track in the queue.
func (b *Bot) cmdSkip(ctx context.Context, roomID string) {
	cur, _ := b.queue.Current()
	b.mu.Lock()
	if b.playCancel != nil {
		b.playCancel()
	}
	b.mu.Unlock()
	if cur.Title != "" {
		b.sendMessage(ctx, roomID, fmt.Sprintf("⏭ Skipped — %s", formatTrack(cur)))
	} else {
		b.sendMessage(ctx, roomID, "⏭ Skipped")
	}
}

// cmdStop clears the queue and cancels playback, leaving the voice call.
func (b *Bot) cmdStop(ctx context.Context, roomID string) {
	n := b.queue.Len()
	b.mu.Lock()
	if b.playCancel != nil {
		b.playCancel()
	}
	b.mu.Unlock()
	b.queue.Clear()
	b.sendMessage(ctx, roomID, fmt.Sprintf("⏹ Stopped · %d track(s) removed", n))
}

// cmdNowPlaying shows the currently playing track and remaining queue count.
func (b *Bot) cmdNowPlaying(ctx context.Context, roomID string) {
	track, ok := b.queue.Current()
	if !ok {
		b.sendMessage(ctx, roomID, "Nothing playing right now.")
		return
	}
	remaining := b.queue.Len()

	b.mu.Lock()
	elapsed := int(time.Since(b.trackStartTime).Seconds())
	b.mu.Unlock()

	b.mu.Lock()
	audioInfo := b.trackAudioInfo
	b.mu.Unlock()

	msg := fmt.Sprintf("Now playing : %s%s [%s/%s] · %s", track.Title, artistSuffix(track.Artist), formatDuration(elapsed), formatDuration(track.Duration), audioInfo)
	if remaining > 1 {
		msg += fmt.Sprintf("\n📋 `%d` more in queue", remaining-1)
	}
	b.sendMessage(ctx, roomID, msg)
}

// cmdVolume shows or sets the playback volume (0–200%).
func (b *Bot) cmdVolume(ctx context.Context, roomID, args string) {
	if args == "" {
		b.mu.Lock()
		v := b.volume
		b.mu.Unlock()
		b.sendMessage(ctx, roomID, fmt.Sprintf("🔊 Volume: `%.0f%%`", v*100))
		return
	}

	pct, err := strconv.Atoi(args)
	if err != nil || pct < 0 || pct > 200 {
		b.sendMessage(ctx, roomID, "Usage: `/volume <0-200>` — set volume percentage")
		return
	}

	v := float64(pct) / 100.0
	b.mu.Lock()
	b.volume = v
	if b.activePlayer != nil {
		b.activePlayer.SetVolume(v)
	}
	b.mu.Unlock()
	b.sendMessage(ctx, roomID, fmt.Sprintf("🔊 Volume set to `%d%%`", pct))
}

// startPlayback begins a goroutine that plays through the queue sequentially.
// It is a no-op if playback is already running.
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

			if err != nil && err != context.Canceled {
				slog.Error("play track error", "error", err)
				b.chattoClient.CreateMessage(context.Background(), roomID, fmt.Sprintf("⚠️ Playback error: %v", err))
			}
		}
	}()
}

// endPlayback marks playback as stopped and sends an empty-queue message.
// The bot stays in the voice call, ready for more tracks.
func (b *Bot) endPlayback(roomID string) {
	b.mu.Lock()
	b.running = false
	b.currentRoom = ""
	b.mu.Unlock()

	b.chattoClient.CreateMessage(context.Background(), roomID, "Queue empty — add more with `/play`")
}

// playTrack plays a single track: joins the voice call, creates a LiveKit
// player, streams the track from Tidal, and blocks until playback completes.
func (b *Bot) playTrack(ctx context.Context, roomID string, track Track) error {
	remaining := b.queue.Len()

	b.mu.Lock()
	b.trackStartTime = time.Now()
	b.mu.Unlock()

	msg := fmt.Sprintf("Now playing : %s%s [0:00/%s] · %s", track.Title, artistSuffix(track.Artist), formatDuration(track.Duration), b.trackAudioInfo)
	if remaining > 0 {
		msg += fmt.Sprintf("\n📋 `%d` more in queue", remaining)
	}
	b.chattoClient.CreateMessage(context.Background(), roomID, msg)

	joined, err := b.chattoClient.JoinCall(ctx, roomID)
	if err != nil {
		return fmt.Errorf("join call: %w", err)
	}
	if !joined {
		b.chattoClient.CreateMessage(context.Background(), roomID, "Join a voice channel first and try again.")
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
	b.activePlayer.SetVolume(b.volume)
	b.mu.Unlock()

	defer func() {
		player.Disconnect()
		b.mu.Lock()
		b.activePlayer = nil
		b.mu.Unlock()
	}()

	slog.Info("starting tidal stream", "tid", track.TID)
	stream, err := b.tidalClient.StreamTrack(ctx, track.TID)
	if err != nil {
		return fmt.Errorf("stream track: %w", err)
	}
	defer stream.Reader.Close()

	b.mu.Lock()
	b.trackAudioInfo = stream.FormatAudioInfo()
	b.mu.Unlock()
	slog.Info("tidal stream ready, starting playback", "duration", stream.Duration, "audio", b.trackAudioInfo)

	return player.Play(ctx, stream.Reader, nil)
}

// leaveCall leaves the voice call in the specified room.
func (b *Bot) leaveCall(ctx context.Context, roomID string) {
	if _, err := b.chattoClient.LeaveCall(ctx, roomID); err != nil {
		slog.Error("leave call", "error", err)
	}
}

// sendMessage sends a chat message, logging any error but not returning it.
func (b *Bot) sendMessage(ctx context.Context, roomID, text string) {
	if err := b.chattoClient.CreateMessage(ctx, roomID, text); err != nil {
		slog.Error("send message", "error", err)
	}
}

// parseEventTime attempts to parse a Chatto timestamp string across
// multiple RFC3339 and common protobuf timestamp formats.
func parseEventTime(s string) time.Time {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05.000000",
		"2006-01-02T15:04:05.000000000",
	}
	for _, fmt := range formats {
		if t, err := time.Parse(fmt, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Shutdown gracefully stops the bot, cancelling any active playback,
// disconnecting the LiveKit player, and closing the Tidal client.
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

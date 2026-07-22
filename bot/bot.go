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

// artistSuffix returns " by <artist>" when artist is non-empty, or an empty string otherwise.
func artistSuffix(artist string) string {
	if artist == "" {
		return ""
	}
	return " by " + artist
}

// formatDuration converts seconds to a "m:ss" string.
func formatDuration(seconds int) string {
	m := seconds / 60
	s := seconds % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatTrack returns a Markdown-formatted track string with title and duration.
func formatTrack(t Track) string {
	return fmt.Sprintf("**%s** — `%s`", t.Title, formatDuration(t.Duration))
}

// Bot is the main bot instance. It manages all rooms, each with its own
// queue and playback state, and orchestrates Tidal streaming and LiveKit publishing.
type Bot struct {
	cfg          *Config
	chattoClient *chatto.Client
	tidalClient  *tidal.Client
	botName      string
	botUserID    string
	startedAt    time.Time

	mu     sync.Mutex
	volume float64

	rooms map[string]*roomState

	seenEvents   map[string]struct{}
	seenEventsMu sync.Mutex
}

// roomState holds per-room playback state so that commands in one room
// never interfere with playback in another room.
type roomState struct {
	mu             sync.Mutex
	queue          *Queue
	running        bool
	activePlayer   *livekit.Player
	playCtx        context.Context
	playCancel     context.CancelFunc
	trackStartTime time.Time
	trackAudioInfo string
}

// Config holds the bot's runtime configuration.
type Config struct {
	ChattoURL      string
	ChattoToken    string
	Rooms          []string
	LivekitURL     string
	TidalTokenPath string
	TidalQuality   string
	SampleRate     int
	PollInterval   time.Duration
	BotName        string
	Volume         int
}

func New(ctx context.Context, cfg *Config, chattoClient *chatto.Client) (*Bot, error) {
	tidalClient, err := tidal.NewClient(ctx, cfg.TidalTokenPath, cfg.TidalQuality)
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

	rooms := make(map[string]*roomState, len(cfg.Rooms))
	for _, id := range cfg.Rooms {
		rooms[id] = &roomState{queue: NewQueue()}
	}

	return &Bot{
		cfg:          cfg,
		chattoClient: chattoClient,
		tidalClient:  tidalClient,
		botName:      botName,
		botUserID:    botUserID,
		startedAt:    time.Now(),
		rooms:        rooms,
		volume:       float64(cfg.Volume) / 100.0,
		seenEvents:   make(map[string]struct{}),
	}, nil
}

// room returns the roomState for a given roomID. The room is guaranteed
// to exist because entries are created in New for each configured room.
func (b *Bot) room(roomID string) *roomState {
	return b.rooms[roomID]
}

// Run starts the bot event loop. It sets presence, then polls all configured
// rooms on a ticker for new events and auto-starts playback when tracks are queued.
func (b *Bot) Run(ctx context.Context) error {
	slog.Info("bot started",
		"rooms", b.cfg.Rooms,
		"poll_interval", b.cfg.PollInterval,
	)

	b.setPresence(ctx)

	cursors := make(map[string]string)

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:

			for _, roomID := range b.cfg.Rooms {
				b.pollRoom(ctx, roomID, cursors)
			}
		}

		b.autoStartPlayback(ctx)
	}
}

// setPresence sets the bot's online status and custom status text on Chatto.
func (b *Bot) setPresence(ctx context.Context) {
	if err := b.chattoClient.UpdatePresence(ctx, "PRESENCE_STATUS_ONLINE", true); err != nil {
		slog.Warn("set online presence", "error", err)
	}
	if err := b.chattoClient.UpdateCustomStatus(ctx, "🎧", "Providing high-res songs"); err != nil {
		slog.Warn("set custom status", "error", err)
	}
}

// pollRoom fetches new timeline events for a room, updates the cursor, and
// processes each event. Errors are handled by handlePollError.
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
		slog.Debug("received events", "room", roomID, "count", len(page.Events), "cursor", page.EndCursor)
	} else {
		slog.Debug("no new events", "room", roomID, "cursor", page.EndCursor)
	}
	for _, event := range page.Events {
		b.processEvent(ctx, roomID, event)
	}
}

// handlePollError handles room polling errors. If the error indicates the bot
// is not a member, it attempts to add itself to the room automatically.
func (b *Bot) handlePollError(ctx context.Context, roomID string, err error, after string) {
	if chatto.IsNotMemberError(err) || chatto.IsPermissionDeniedError(err) {
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
		if _, err := b.chattoClient.GetRoomEvents(ctx, roomID, after, 50); err != nil {
			slog.Error("poll room events after join", "room", roomID, "error", err)
		}
	} else {
		slog.Error("poll room events", "room", roomID, "error", err)
	}
}

// processEvent handles a single room event. It filters out own messages,
// events that occurred before the bot started, and already-seen events.
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

// autoStartPlayback checks each room for a non-empty queue and starts playback
// if the room is not already playing.
func (b *Bot) autoStartPlayback(ctx context.Context) {
	for _, roomID := range b.cfg.Rooms {
		rs := b.room(roomID)
		rs.mu.Lock()
		running := rs.running
		rs.mu.Unlock()
		if !running && rs.queue.Len() > 0 {
			b.startPlayback(ctx, roomID)
		}
	}
}

// handleMessage parses a message body for commands and dispatches to the
// appropriate handler method.
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

// cmdHelp sends a list of available commands to the room.
func (b *Bot) cmdHelp(ctx context.Context, roomID string) {
	b.sendMessage(ctx, roomID, "**Tidal Bot** — Commands\n\n`play <track>` — Play a track\n`queue <track>` — Add to queue\n`queue` — View queue\n`skip` — Skip to next\n`stop` — Stop & clear queue\n`nowplaying` — Now playing\n`volume <0-200>` — Set volume\n`help` — This message")
}

// cmdTest joins the voice call, publishes 10 seconds of silence, then leaves.
// It verifies LiveKit connectivity without playing actual audio.
func (b *Bot) cmdTest(ctx context.Context, roomID string) {
	b.sendMessage(ctx, roomID, "Testing LiveKit connection...")

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
			URL:        b.cfg.LivekitURL,
			Token:      token.Token,
			Room:       roomID,
			SampleRate: b.cfg.SampleRate,
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
			b.sendMessage(ctx, roomID, "LiveKit connection OK · 10s silence")
		}
	}()
}

// resolveTracks searches Tidal for tracks matching a query string.
// Only text search is supported; URL-based resolution was removed.
func (b *Bot) resolveTracks(ctx context.Context, query string) ([]tidal.SearchResult, error) {
	results, err := b.tidalClient.Search(ctx, query, 5)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results, nil
}

// cmdPlay searches for a track by name and queues the first result.
// If multiple results are found they are shown and the first is queued.
func (b *Bot) cmdPlay(ctx context.Context, roomID, actorID, query string) {
	if query == "" {
		b.sendMessage(ctx, roomID, "Usage: `play <track name>` — e.g. `play Daft Punk Around the World`")
		return
	}

	tracks, err := b.resolveTracks(ctx, query)
	if err != nil {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(tracks) == 0 {
		b.sendMessage(ctx, roomID, fmt.Sprintf("No results for: %s", query))
		return
	}

	if len(tracks) > 1 {
		var msg strings.Builder
		msg.WriteString(fmt.Sprintf("Top results for \"**%s**\":\n", query))
		for i, r := range tracks {
			msg.WriteString(fmt.Sprintf("`%d.` %s\n", i+1, formatTrack(Track{Title: r.Title, Artist: r.Artist, Duration: r.Duration})))
		}
		msg.WriteString(fmt.Sprintf("\nPlaying first result: %s", formatTrack(Track{Title: tracks[0].Title, Artist: tracks[0].Artist, Duration: tracks[0].Duration})))
		b.sendMessage(ctx, roomID, msg.String())
	}
	tracks = tracks[:1]

	b.addTracksToQueue(ctx, roomID, actorID, tracks)
}

// cmdQueue searches for a track by name and appends the first result to the
// end of the queue. With no arguments, it shows the current queue.
func (b *Bot) cmdQueue(ctx context.Context, roomID, actorID, query string) {
	if query == "" {
		b.printQueue(ctx, roomID)
		return
	}

	tracks, err := b.resolveTracks(ctx, query)
	if err != nil {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(tracks) == 0 {
		b.sendMessage(ctx, roomID, fmt.Sprintf("No results for: %s", query))
		return
	}

	tracks = tracks[:1]

	b.addTracksToQueue(ctx, roomID, actorID, tracks)
}

// printQueue sends the current queue listing (with total duration) to the room.
func (b *Bot) printQueue(ctx context.Context, roomID string) {
	rs := b.room(roomID)
	tracks := rs.queue.List()
	if rs.queue.Len() == 0 {
		b.sendMessage(ctx, roomID, "Queue is empty.")
		return
	}
	total := 0
	for _, t := range tracks {
		total += t.Duration
	}
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("**Queue** · %d tracks · `%s`\n", len(tracks), formatDuration(total)))
	for i, t := range tracks {
		msg.WriteString(fmt.Sprintf("`%d.` %s\n", i+1, formatTrack(t)))
	}
	b.sendMessage(ctx, roomID, msg.String())
}

// addTracksToQueue appends one or more tracks to the room's queue and sends
// a confirmation message to the room.
func (b *Bot) addTracksToQueue(ctx context.Context, roomID, actorID string, tracks []tidal.SearchResult) {
	rs := b.room(roomID)

	for _, r := range tracks {
		rs.queue.Add(Track{
			TID:       r.ID,
			Title:     r.Title,
			Artist:    r.Artist,
			Duration:  r.Duration,
			Requestor: actorID,
		})
	}

	if len(tracks) == 1 {
		r := tracks[0]
		pos := rs.queue.Len()

		total := 0
		for _, t := range rs.queue.List() {
			total += t.Duration
		}
		eta := ""
		if pos > 1 {
			eta = fmt.Sprintf("· ~%s until play", formatDuration(total))
		}

		msg := fmt.Sprintf("Added #**%d** — %s", pos, formatTrack(Track{Title: r.Title, Artist: r.Artist, Duration: r.Duration}))
		if eta != "" {
			msg += " " + eta
		}
		b.sendMessage(ctx, roomID, msg)
	} else {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Added **%d** tracks to queue", len(tracks)))
	}
}

// cmdSkip cancels the current playback context, causing the playback loop to
// advance to the next track in the queue.
func (b *Bot) cmdSkip(ctx context.Context, roomID string) {
	rs := b.room(roomID)
	cur, _ := rs.queue.Current()
	rs.mu.Lock()
	if rs.playCancel != nil {
		rs.playCancel()
	}
	rs.mu.Unlock()
	if cur.Title != "" {
		b.sendMessage(ctx, roomID, fmt.Sprintf("Skipped — %s", formatTrack(cur)))
	} else {
		b.sendMessage(ctx, roomID, "Skipped")
	}
}

// cmdStop cancels playback, clears the queue, disconnects from LiveKit,
// and leaves the voice call.
func (b *Bot) cmdStop(ctx context.Context, roomID string) {
	rs := b.room(roomID)
	n := rs.queue.Len()
	rs.mu.Lock()
	if rs.playCancel != nil {
		rs.playCancel()
	}
	player := rs.activePlayer
	rs.activePlayer = nil
	rs.running = false
	rs.mu.Unlock()
	rs.queue.Clear()

	if player != nil {
		player.Disconnect()
	}

	b.leaveCall(ctx, roomID)
	b.sendMessage(ctx, roomID, fmt.Sprintf("Stopped · %d track(s) removed", n))
}

// cmdNowPlaying sends the currently playing track title, artist, elapsed time,
// total duration, and audio quality info to the room.
func (b *Bot) cmdNowPlaying(ctx context.Context, roomID string) {
	rs := b.room(roomID)
	track, ok := rs.queue.Current()
	if !ok {
		b.sendMessage(ctx, roomID, "Nothing playing right now.")
		return
	}
	remaining := rs.queue.Len()

	rs.mu.Lock()
	elapsed := int(time.Since(rs.trackStartTime).Seconds())
	audioInfo := rs.trackAudioInfo
	rs.mu.Unlock()

	msg := fmt.Sprintf("Now playing : %s%s [%s/%s] · %s", track.Title, artistSuffix(track.Artist), formatDuration(elapsed), formatDuration(track.Duration), audioInfo)
	if remaining > 1 {
		msg += fmt.Sprintf("\n`%d` more in queue", remaining-1)
	}
	b.sendMessage(ctx, roomID, msg)
}

// cmdVolume shows the current volume setting, or sets it globally (0–200%)
// across all rooms. Changes take effect immediately on the active player.
func (b *Bot) cmdVolume(ctx context.Context, roomID, args string) {
	if args == "" {
		b.mu.Lock()
		v := b.volume
		b.mu.Unlock()
		b.sendMessage(ctx, roomID, fmt.Sprintf("Volume: `%.0f%%`", v*100))
		return
	}

	pct, err := strconv.Atoi(args)
	if err != nil || pct < 0 || pct > 200 {
		b.sendMessage(ctx, roomID, "Usage: `volume <0-200>` — set volume percentage")
		return
	}

	v := float64(pct) / 100.0
	b.mu.Lock()
	b.volume = v
	for _, rs := range b.rooms {
		rs.mu.Lock()
		if rs.activePlayer != nil {
			rs.activePlayer.SetVolume(v)
		}
		rs.mu.Unlock()
	}
	b.mu.Unlock()
	b.sendMessage(ctx, roomID, fmt.Sprintf("Volume set to `%d%%`", pct))
}

// startPlayback begins a goroutine that iterates through the room's queue,
// playing each track sequentially. It sets running to true to prevent
// concurrent playback loops.
func (b *Bot) startPlayback(ctx context.Context, roomID string) {
	rs := b.room(roomID)

	rs.mu.Lock()
	if rs.running {
		rs.mu.Unlock()
		return
	}
	rs.running = true
	rs.mu.Unlock()

	go func() {
		playLoopCtx, loopCancel := context.WithCancel(ctx)
		defer loopCancel()

		for {
			track, ok := rs.queue.Next()
			if !ok {
				b.endPlayback(playLoopCtx, roomID)
				return
			}

			playCtx, cancel := context.WithCancel(playLoopCtx)
			rs.mu.Lock()
			rs.playCtx = playCtx
			rs.playCancel = cancel
			rs.mu.Unlock()

			err := b.playTrack(playCtx, roomID, track)
			cancel()

			if err != nil && err != context.Canceled {
				slog.Error("play track error", "error", err)
				b.sendMessage(playLoopCtx, roomID, fmt.Sprintf("Playback error: %v", err))
			}
		}
	}()
}

// endPlayback stops playback, disconnects the LiveKit player, and notifies
// the room that the queue is empty. The bot stays in the voice call for
// subsequent tracks.
func (b *Bot) endPlayback(ctx context.Context, roomID string) {
	rs := b.room(roomID)
	rs.mu.Lock()
	rs.running = false
	player := rs.activePlayer
	rs.mu.Unlock()

	if player != nil {
		player.Disconnect()
		rs.mu.Lock()
		rs.activePlayer = nil
		rs.mu.Unlock()
	}

	b.sendMessage(ctx, roomID, "Queue empty — add more with `play`")
}

// playTrack streams a track from Tidal and publishes it to LiveKit via ffmpeg.
// It ensures a LiveKit connection exists (joining the voice call if necessary),
// then decodes the audio and writes PCM16 samples to the track.
func (b *Bot) playTrack(ctx context.Context, roomID string, track Track) error {
	rs := b.room(roomID)
	remaining := rs.queue.Len()

	rs.mu.Lock()
	rs.trackStartTime = time.Now()
	rs.mu.Unlock()

	msg := fmt.Sprintf("Now playing : %s%s [0:00/%s] · %s", track.Title, artistSuffix(track.Artist), formatDuration(track.Duration), rs.trackAudioInfo)
	if remaining > 0 {
		msg += fmt.Sprintf("\n`%d` more in queue", remaining)
	}
	b.sendMessage(ctx, roomID, msg)

	// Ensure we have a LiveKit player connected.
	rs.mu.Lock()
	player := rs.activePlayer
	rs.mu.Unlock()

	if player == nil {
		joined, err := b.chattoClient.JoinCall(ctx, roomID)
		if err != nil {
			return fmt.Errorf("join call: %w", err)
		}
		if !joined {
			b.sendMessage(ctx, roomID, "Join a voice channel first and try again.")
			return nil
		}

		token, err := b.chattoClient.GetCallToken(ctx, roomID)
		if err != nil {
			return fmt.Errorf("get call token: %w", err)
		}

		player, err = livekit.NewPlayer(livekit.Config{
			URL:        b.cfg.LivekitURL,
			Token:      token.Token,
			Room:       roomID,
			SampleRate: b.cfg.SampleRate,
		})
		if err != nil {
			return fmt.Errorf("create livekit player: %w", err)
		}

		rs.mu.Lock()
		rs.activePlayer = player
		player.SetVolume(b.volume)
		rs.mu.Unlock()
	}

	slog.Info("starting tidal stream", "tid", track.TID)
	stream, err := b.tidalClient.StreamTrack(ctx, track.TID)
	if err != nil {
		return fmt.Errorf("stream track: %w", err)
	}
	defer stream.Reader.Close()

	rs.mu.Lock()
	rs.trackAudioInfo = stream.FormatAudioInfo()
	rs.mu.Unlock()
	slog.Info("tidal stream ready, starting playback", "duration", stream.Duration, "audio", rs.trackAudioInfo)

	return player.Play(ctx, stream.Reader, nil)
}

// leaveCall leaves the voice call in the specified room.
func (b *Bot) leaveCall(ctx context.Context, roomID string) {
	if _, err := b.chattoClient.LeaveCall(ctx, roomID); err != nil {
		slog.Error("leave call", "error", err)
	}
}

// sendMessage sends a text message to a room.
func (b *Bot) sendMessage(ctx context.Context, roomID, text string) {
	if err := b.chattoClient.CreateMessage(ctx, roomID, text); err != nil {
		slog.Error("send message", "error", err)
	}
}

// parseEventTime parses a timestamp string using multiple RFC 3339 variants.
// Returns the zero time if none of the formats match.
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

func (b *Bot) Shutdown() {
	slog.Info("shutting down bot...")
	for _, rs := range b.rooms {
		rs.mu.Lock()
		if rs.playCancel != nil {
			rs.playCancel()
		}
		if rs.activePlayer != nil {
			rs.activePlayer.Disconnect()
		}
		rs.mu.Unlock()
	}
	b.tidalClient.Close()
}

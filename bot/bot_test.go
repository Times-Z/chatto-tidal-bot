package bot

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds int
		want    string
	}{
		{0, "0:00"},
		{59, "0:59"},
		{60, "1:00"},
		{247, "4:07"},
		{3661, "61:01"},
	}

	for _, tc := range tests {
		got := formatDuration(tc.seconds)
		if got != tc.want {
			t.Fatalf("formatDuration(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestFormatTrack(t *testing.T) {
	tr := Track{Title: "Around the World", Artist: "Daft Punk", Duration: 247}
	got := formatTrack(tr)
	want := "**Around the World** — `4:07`"
	if got != want {
		t.Fatalf("formatTrack = %q, want %q", got, want)
	}
}

func TestParseEventTime(t *testing.T) {
	tests := []struct {
		input string
		want  time.Time
	}{
		{"2026-07-22T12:40:29.068Z", time.Date(2026, 7, 22, 12, 40, 29, 68_000_000, time.UTC)},
		{"2026-07-22T12:40:29Z", time.Date(2026, 7, 22, 12, 40, 29, 0, time.UTC)},
		{"2026-07-22T12:40:29", time.Date(2026, 7, 22, 12, 40, 29, 0, time.UTC)},
		{"2026-07-22T12:40:29.000", time.Date(2026, 7, 22, 12, 40, 29, 0, time.UTC)},
		{"not a time", time.Time{}},
		{"", time.Time{}},
	}

	for _, tc := range tests {
		got := parseEventTime(tc.input)
		if !got.Equal(tc.want) {
			t.Fatalf("parseEventTime(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestStripMention(t *testing.T) {
	tests := []struct {
		body        string
		botName     string
		wantBody    string
		wantMention bool
	}{
		{"@tidal.bot play bad", "tidal.bot", "play bad", true},
		{"@Tidal.Bot play bad", "tidal.bot", "play bad", true},
		{"play bad", "tidal.bot", "play bad", false},
		{"@other.bot play", "tidal.bot", "@other.bot play", false},
		{"@tidal.bot", "tidal.bot", "", true},
		{"", "tidal.bot", "", false},
		{"@tidal.bot  play  bad", "tidal.bot", "play  bad", true},
		{"@tidal.bot/play", "tidal.bot", "/play", true},
	}

	for _, tc := range tests {
		body, mentioned := stripMention(tc.body, tc.botName)
		if body != tc.wantBody {
			t.Fatalf("stripMention(%q, %q) body = %q, want %q", tc.body, tc.botName, body, tc.wantBody)
		}
		if mentioned != tc.wantMention {
			t.Fatalf("stripMention(%q, %q) mentioned = %v, want %v", tc.body, tc.botName, mentioned, tc.wantMention)
		}
	}
}

func TestStripMentionEmptyBotName(t *testing.T) {
	body, mentioned := stripMention("@tidal.bot play", "")
	if body != "@tidal.bot play" {
		t.Fatalf("expected body unchanged, got %q", body)
	}
	if mentioned {
		t.Fatal("expected no mention")
	}
}

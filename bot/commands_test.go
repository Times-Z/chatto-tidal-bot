package bot

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		botName  string
		wantCmd  Command
		wantArgs string
		wantNil  bool
	}{
		{
			name:     "plain command",
			body:     "play Around The World",
			botName:  "tidal.bot",
			wantCmd:  CmdPlay,
			wantArgs: "Around The World",
		},
		{
			name:     "slash command",
			body:     "/queue Daft Punk",
			botName:  "tidal.bot",
			wantCmd:  CmdQueue,
			wantArgs: "Daft Punk",
		},
		{
			name:     "mention and slash",
			body:     "@tidal.bot /skip",
			botName:  "tidal.bot",
			wantCmd:  CmdSkip,
			wantArgs: "",
		},
		{
			name:     "mention and plain",
			body:     "@tidal.bot nowplaying",
			botName:  "tidal.bot",
			wantCmd:  CmdNowPlaying,
			wantArgs: "",
		},
		{
			name:     "case insensitive command",
			body:     "VoLuMe 120",
			botName:  "tidal.bot",
			wantCmd:  CmdVolume,
			wantArgs: "120",
		},
		{
			name:    "empty body",
			body:    "   ",
			botName: "tidal.bot",
			wantNil: true,
		},
		{
			name:    "unknown command",
			body:    "dance now",
			botName: "tidal.bot",
			wantNil: true,
		},
		{
			name:    "only mention",
			body:    "@tidal.bot",
			botName: "tidal.bot",
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCommand(tc.body, tc.botName)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", *got)
				}
				return
			}

			if got == nil {
				t.Fatalf("expected command %q, got nil", tc.wantCmd)
			}
			if got.Command != tc.wantCmd {
				t.Fatalf("command mismatch: got %q want %q", got.Command, tc.wantCmd)
			}
			if got.Args != tc.wantArgs {
				t.Fatalf("args mismatch: got %q want %q", got.Args, tc.wantArgs)
			}
		})
	}
}

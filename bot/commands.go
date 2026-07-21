package bot

import (
	"strings"
)

type Command string

const (
	CmdPlay       Command = "/play"
	CmdQueue      Command = "/queue"
	CmdSkip       Command = "/skip"
	CmdStop       Command = "/stop"
	CmdNowPlaying Command = "/nowplaying"
	CmdHelp       Command = "/help"
)

var allCommands = []Command{CmdPlay, CmdQueue, CmdSkip, CmdStop, CmdNowPlaying, CmdHelp}

type ParsedCommand struct {
	Command Command
	Args    string
}

func parseCommand(body string) *ParsedCommand {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "/") {
		return nil
	}

	parts := strings.SplitN(body, " ", 2)
	cmd := Command(strings.ToLower(parts[0]))
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	for _, c := range allCommands {
		if cmd == c {
			return &ParsedCommand{Command: c, Args: args}
		}
	}

	return nil
}

package bot

import "strings"

// Command represents a bot command identifier (always includes the leading "/").
type Command string

const (
	CmdPlay       Command = "play"
	CmdQueue      Command = "queue"
	CmdSkip       Command = "skip"
	CmdStop       Command = "stop"
	CmdNowPlaying Command = "nowplaying"
	CmdVolume     Command = "volume"
	CmdTest       Command = "test"
	CmdHelp       Command = "help"
)

// allCommands is the list of known commands used for validation in parseCommand.
var allCommands = []Command{CmdPlay, CmdQueue, CmdSkip, CmdStop, CmdNowPlaying, CmdVolume, CmdTest, CmdHelp}

// ParsedCommand holds the result of parsing a chat message into a command and its arguments.
type ParsedCommand struct {
	Command Command
	Args    string
}

// parseCommand extracts a command and its arguments from a chat message body.
//
// It supports two styles:
//   - Slash commands: "/play Daft Punk" or "@bot /play Daft Punk"
//   - Natural language with mention: "@bot play Daft Punk" (without leading "/")
//
// Messages without a recognized command pattern return nil.
func parseCommand(body string, botName string) *ParsedCommand {
	body = strings.TrimSpace(body)

	body, wasMentioned := stripMention(body, botName)

	parts := strings.SplitN(body, " ", 2)
	raw := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	cmd := Command(raw)
	for _, c := range allCommands {
		if cmd == c {
			return &ParsedCommand{Command: c, Args: args}
		}
	}

	if wasMentioned {
		cmd = Command("/" + raw)
		for _, c := range allCommands {
			if cmd == c {
				return &ParsedCommand{Command: c, Args: args}
			}
		}
	}

	return nil
}

// stripMention removes a leading "@botName" prefix from the message body.
// Returns the stripped body and a boolean indicating whether a mention was found.
func stripMention(body, botName string) (string, bool) {
	if botName == "" {
		return body, false
	}
	prefix := "@" + botName
	if strings.HasPrefix(strings.ToLower(body), strings.ToLower(prefix)) {
		body = strings.TrimSpace(body[len(prefix):])
		return body, true
	}
	return body, false
}

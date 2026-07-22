# Chatto Tidal Bot

A music bot for [Chatto](https://github.com/chattocorp/chatto) that plays **Tidal HiFi Plus** streams in voice channels.

## Architecture

```
User ──(/play Daft Punk)──▶ Chatto ──▶ Bot (polling GetRoomEvents)
                                              │
                                              ├──▶ Tidal API (search + FLAC audio stream)
                                              │
                                              ├──▶ Chatto VoiceCallService
                                              │     (JoinCall → GetCallToken)
                                              │
                                              └──▶ LiveKit (publish PCM16 → Opus audio)
                                                     │
                                                     └──▶ Participants hear the music
```

## Prerequisites

- A deployed Chatto server with LiveKit configured
- A **Tidal HiFi Plus** account
- `ffmpeg` installed on the bot machine

## Build

```bash
go build -o chatto-tidal-bot .
```

## Configuration

Copy `config.example.json` → `config.json` and fill in the fields:

```json
{
    "chatto_url": "https://chat.example.com",
    "chatto_token": "cht_...",
    "livekit_url": "wss://livekit.example.com",
    "tidal_token_path": "tidal_token.json",
    "bot_name": "tidal.bot",
    "rooms": [
        "R1YR23T6P9wamep"
    ],
    "poll_interval": "3s",
    "volume": 60
}
```

### `chatto_token` — Bot bearer token

**1. Create the bot account** on the Chatto server:

```bash
# List existing users
sudo -u chatto chatto operator user list

# Set a password for the bot
sudo -u chatto chatto operator user set-password usr_xxxxxxxxxxxx

# Optionally grant permissions
sudo -u chatto chatto operator user role add usr_xxxxxxxxxxxx moderator

# Docker Compose equivalent:
docker compose exec -u chatto chatto /chatto operator user set-password usr_xxxxxxxxxxxx
```

**2. Obtain the token**:

```bash
curl -X POST https://your-instance.chatto.run/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "login": "bot_username",
    "password": "the_password"
  }'
```

The response contains the token in the `"token"` field. You can also grab it from the browser DevTools (Application → Local Storage → `chatto_bearer_token`) after logging into the web UI as the bot.

### `bot_name` — Bot display name

The bot's username on your Chatto server. This is used to recognize mentions — messages like `@tidal.bot /play ...` will trigger the bot. If omitted, defaults to `"tidal.bot"`.

### `chatto_url` — API endpoint

The base URL of your Chatto server (e.g. `https://chat.example.com`). Do **not** include a trailing slash.

### `livekit_url` — LiveKit server URL

Defined in your Chatto configuration (`LIVEKIT_HOST` env var or config file). The bot connects via JWT tokens from `GetCallToken`. Format: `wss://livekit.domain.com` or `ws://IP:7880`.

### `tidal_token_path` — Tidal auth token

The token is obtained automatically on first launch via Tidal's **device authorization flow**:

```bash
./chatto-tidal-bot config.json
```

The bot prints a URL and a code. Open the URL in a browser, enter the code, and authorize Tidal access. The token is saved to the specified file (default: `tidal_token.json`).

### `rooms` — Room IDs

The bot must be an **explicit member** of each room to poll events. Add the bot user to the room through the Chatto web UI (Room settings → Members → Add user) or via API:

To find a room ID, simply open the room in your browser — the ID is in the URL: `https://chat.example.com/rooms/<room_id>`.

> **Note**: The bot automatically tries to add itself to the room on startup by calling `GetViewer` → `AddMember`. This only works if the bot user has `manage` permission on the room.

### `volume` — Default playback volume (0–200)

Initial volume percentage. Can be changed at runtime with `/volume`. Default: `100`.

## Usage

```bash
./chatto-tidal-bot [config.json]
```

The bot joins the configured rooms and listens for chat commands. Defaults to `config.json` if no path is given.

| Command | Description |
|---------|-------------|
| `play <query/URL>` | Search or use a Tidal URL (track, album, playlist, artist) |
| `queue <query/URL>` | Add a track to the queue |
| `queue` | Show the current queue |
| `skip` | Skip to the next track |
| `stop` | Stop playback and clear the queue |
| `nowplaying` | Show the currently playing track |
| `volume <0-200>` | Show or set the volume |
| `help` | Display available commands |

The bot auto-joins the voice call when a track starts playing and stays in the call after the queue empties, ready for more tracks. Use `/stop` to leave the call.

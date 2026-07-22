# Chatto Tidal Bot

A music bot for [Chatto](https://github.com/chattocorp/chatto) v0.4.14 that plays **Tidal HiFi Plus** streams in voice channels via LiveKit.

## Architecture

```
User ──(play Daft Punk)──▶ Chatto ──▶ Bot (polling GetRoomEvents)
                                            │
                                            ├──▶ Tidal API (search + FLAC audio stream)
                                            │
                                            ├──▶ Chatto VoiceCallService
                                            │     (JoinCall → GetCallToken)
                                            │
                                            └──▶ LiveKit (publish PCM16 audio)
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

# or

task build
```

## Development Tasks

This project uses [Task](https://taskfile.dev) (task.dev) for common developer workflows.

```bash
task --list
task fmt
task test
task test:nocgo
```

### Build Requirements

The default build (CGO enabled) requires native audio libraries used by LiveKit/media-sdk,
including `soxr` and `opusfile`.

If these system dependencies are not available, you can still run most unit tests using:

```bash
task test:nocgo
```

In this mode, the `livekit` package is compiled with a no-CGO stub player that returns a
clear runtime error when audio publishing is attempted.

## Configuration

Copy `config.example.json` → `config.json` and fill in the fields:

```json
{
    "chatto_url": "https://chat.example.com",
    "chatto_token": "cht_...",
    "livekit_url": "wss://livekit.example.com",
    "tidal_token_path": "tidal_token.json",
    "tidal_quality": "HI_RES_LOSSLESS",
    "sample_rate": 48000,
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
chatto operator user create --login bot_username --password "the_password"
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

The bot's username on your Chatto server. Used to recognize mentions — messages like `@tidal.bot play ...` will trigger the bot. If omitted, defaults to `"tidal.bot"`.

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

### `tidal_quality` — Stream audio quality

Controls the audio quality requested from Tidal. Valid values:

| Value | Description |
|-------|-------------|
| `"LOW"` | Low bitrate (AAC) |
| `"HIGH"` | High bitrate (AAC) |
| `"LOSSLESS"` | CD-quality FLAC (16bit 44.1kHz) |
| `"HI_RES_LOSSLESS"` | Hi-Res FLAC (up to 24bit 192kHz) |

If omitted or set to an unrecognized value, the bot picks the **best quality available** per track.

### `sample_rate` — Audio output sample rate

Sample rate in Hz for the LiveKit PCM audio track and ffmpeg output. Must be supported by LiveKit (common values: `44100`, `48000`). Defaults to `48000`.

> **Note**: LiveKit only supports 16-bit PCM audio. Setting a higher sample rate (e.g. `192000`) does not increase audible quality — the audio is always resampled to 48kHz Opus before reaching listeners. Use the default `48000` for best compatibility, or `44100` to slightly reduce bandwidth.

### `rooms` — Room IDs

The bot must be an **explicit member** of each room to poll events. Add the bot user to the room through the Chatto web UI (Room settings → Members → Add user) or via API:

To find a room ID, simply open the room in your browser — the ID is in the URL: `https://chat.example.com/rooms/<room_id>`.

> **Note**: The bot automatically tries to add itself to the room on startup by calling `GetViewer` → `AddMember`. This only works if the bot user has `manage` permission on the room.

### `volume` — Default playback volume (0–200)

Initial volume percentage. Can be changed at runtime with `volume`. Default: `20`.

## Usage

```bash
./chatto-tidal-bot [config.json]
```

The bot joins the configured rooms and listens for chat commands. Defaults to `config.json` if no path is given.

| Command | Description |
|---------|-------------|
| `play <query>` | Search and play a track (text search only) |
| `queue <query>` | Add a track to the queue |
| `queue` | Show the current queue |
| `skip` | Skip to the next track |
| `stop` | Stop playback and clear the queue |
| `nowplaying` | Show the currently playing track |
| `volume <0-200>` | Show or set the global volume |
| `help` | Display available commands |

Commands work with or without a leading `/`. When the bot is mentioned (e.g. `@tidal.bot play ...`), the `/` may be omitted entirely.

The bot auto-joins the voice call when a track starts playing and stays in the call after the queue empties, ready for more tracks. Use `/stop` to leave the call.

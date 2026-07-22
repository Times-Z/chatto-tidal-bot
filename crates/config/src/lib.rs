use serde::{Deserialize, Serialize};
use std::env;
use std::fs;
use std::path::Path;
use std::time::Duration;
use thiserror::Error;
use url::Url;

const DEFAULT_POLL_INTERVAL: Duration = Duration::from_secs(3);
const DEFAULT_SAMPLE_RATE: u32 = 48_000;
const DEFAULT_VOLUME: u8 = 20;
const DEFAULT_TIDAL_TOKEN_PATH: &str = "tidal_token.json";

#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("read config: {0}")]
    Read(std::io::Error),
    #[error("parse config: {0}")]
    Parse(serde_json::Error),
    #[error("invalid config: {0}")]
    Validation(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AppConfig {
    pub chatto_url: String,
    pub chatto_token: String,
    pub tidal_token_path: String,
    pub tidal_quality: String,
    pub sample_rate: u32,
    pub livekit_url: String,
    pub rooms: Vec<String>,
    pub poll_interval: Duration,
    pub bot_name: String,
    pub volume: u8,
}

#[derive(Debug, Deserialize, Serialize)]
struct RawConfig {
    #[serde(default)]
    chatto_url: String,
    #[serde(default)]
    chatto_token: String,
    #[serde(default)]
    tidal_token_path: String,
    #[serde(default)]
    tidal_quality: String,
    #[serde(default)]
    sample_rate: Option<u32>,
    #[serde(default)]
    livekit_url: String,
    #[serde(default)]
    rooms: Vec<String>,
    #[serde(default)]
    poll_interval: Option<String>,
    #[serde(default)]
    bot_name: String,
    #[serde(default)]
    volume: Option<u16>,
}

impl AppConfig {
    pub fn load_from_path(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        let data = fs::read_to_string(path).map_err(ConfigError::Read)?;
        let mut raw: RawConfig = serde_json::from_str(&data).map_err(ConfigError::Parse)?;

        if raw.chatto_url.trim().is_empty() {
            raw.chatto_url = env::var("CHATTO_URL").unwrap_or_default();
        }
        if raw.chatto_token.trim().is_empty() {
            raw.chatto_token = env::var("CHATTO_TOKEN").unwrap_or_default();
        }
        if raw.tidal_token_path.trim().is_empty() {
            raw.tidal_token_path = DEFAULT_TIDAL_TOKEN_PATH.to_owned();
        }

        let poll_interval = raw
            .poll_interval
            .as_deref()
            .map(parse_duration)
            .transpose()?
            .unwrap_or(DEFAULT_POLL_INTERVAL);

        let volume_u16 = raw.volume.unwrap_or(u16::from(DEFAULT_VOLUME));
        let volume = u8::try_from(volume_u16)
            .map_err(|_| ConfigError::Validation("volume must be between 0 and 200".to_owned()))?;

        let config = AppConfig {
            chatto_url: raw.chatto_url,
            chatto_token: raw.chatto_token,
            tidal_token_path: raw.tidal_token_path,
            tidal_quality: raw.tidal_quality,
            sample_rate: raw.sample_rate.unwrap_or(DEFAULT_SAMPLE_RATE),
            livekit_url: raw.livekit_url,
            rooms: raw.rooms,
            poll_interval,
            bot_name: raw.bot_name,
            volume,
        };

        config.validate()?;
        Ok(config)
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        if self.chatto_url.trim().is_empty() {
            return Err(ConfigError::Validation("chatto_url is required".to_owned()));
        }
        validate_url(&self.chatto_url, "chatto_url", &["http", "https"])?;

        if self.chatto_token.trim().is_empty() {
            return Err(ConfigError::Validation(
                "chatto_token is required".to_owned(),
            ));
        }

        if self.livekit_url.trim().is_empty() {
            return Err(ConfigError::Validation(
                "livekit_url is required".to_owned(),
            ));
        }
        validate_url(&self.livekit_url, "livekit_url", &["ws", "wss"])?;

        if self.rooms.is_empty() {
            return Err(ConfigError::Validation(
                "rooms must contain at least one room ID".to_owned(),
            ));
        }

        if let Some((idx, _)) = self
            .rooms
            .iter()
            .enumerate()
            .find(|(_, room)| room.trim().is_empty())
        {
            return Err(ConfigError::Validation(format!(
                "rooms[{idx}] must not be empty"
            )));
        }

        if self.poll_interval.is_zero() {
            return Err(ConfigError::Validation(
                "poll_interval must be > 0".to_owned(),
            ));
        }

        if self.volume > 200 {
            return Err(ConfigError::Validation(
                "volume must be between 0 and 200".to_owned(),
            ));
        }

        if self.sample_rate != 44_100 && self.sample_rate != 48_000 {
            return Err(ConfigError::Validation(
                "sample_rate must be 44100 or 48000".to_owned(),
            ));
        }

        if self.tidal_token_path.trim().is_empty() {
            return Err(ConfigError::Validation(
                "tidal_token_path must not be empty".to_owned(),
            ));
        }

        Ok(())
    }
}

fn parse_duration(input: &str) -> Result<Duration, ConfigError> {
    humantime::parse_duration(input)
        .map_err(|_| ConfigError::Validation("poll_interval must be a valid duration".to_owned()))
}

fn validate_url(raw: &str, field: &str, allowed_schemes: &[&str]) -> Result<(), ConfigError> {
    let parsed = Url::parse(raw)
        .map_err(|err| ConfigError::Validation(format!("{field} is not a valid URL: {err}")))?;

    if parsed.scheme().is_empty() || parsed.host_str().is_none() {
        return Err(ConfigError::Validation(format!(
            "{field} must include scheme and host"
        )));
    }

    if allowed_schemes
        .iter()
        .any(|scheme| parsed.scheme().eq_ignore_ascii_case(scheme))
    {
        return Ok(());
    }

    Err(ConfigError::Validation(format!(
        "{field} must use one of schemes: {}",
        allowed_schemes.join(", ")
    )))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn valid_config() -> AppConfig {
        AppConfig {
            chatto_url: "https://chat.example.com".to_owned(),
            chatto_token: "cht_token".to_owned(),
            tidal_token_path: "tidal_token.json".to_owned(),
            tidal_quality: String::new(),
            sample_rate: 48_000,
            livekit_url: "wss://livekit.example.com".to_owned(),
            rooms: vec!["room1".to_owned()],
            poll_interval: Duration::from_secs(3),
            bot_name: String::new(),
            volume: 20,
        }
    }

    #[test]
    fn config_validate() {
        let mut cfg = valid_config();
        assert!(cfg.validate().is_ok());

        cfg.chatto_url.clear();
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.chatto_url = "ftp://chat.example.com".to_owned();
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.chatto_token.clear();
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.livekit_url.clear();
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.livekit_url = "https://livekit.example.com".to_owned();
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.rooms.clear();
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.rooms = vec!["room1".to_owned(), "   ".to_owned()];
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.poll_interval = Duration::ZERO;
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.volume = 201;
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.sample_rate = 96_000;
        assert!(cfg.validate().is_err());
        cfg = valid_config();

        cfg.tidal_token_path.clear();
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn parse_duration_valid() {
        assert_eq!(parse_duration("3s").unwrap(), Duration::from_secs(3));
        assert_eq!(parse_duration("1m").unwrap(), Duration::from_secs(60));
        assert_eq!(parse_duration("500ms").unwrap(), Duration::from_millis(500));
    }

    #[test]
    fn parse_duration_invalid() {
        assert!(parse_duration("not-a-duration").is_err());
    }
}

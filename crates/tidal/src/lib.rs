#![deny(unsafe_code)]

use base64::Engine;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Client as HttpClient, Method, StatusCode};
use serde::{Deserialize, Serialize};
use std::fmt;
use std::fs;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use thiserror::Error;
use tokio::time::sleep;
use url::Url;

const TIDAL_API_BASE: &str = "https://api.tidal.com/v1";
const DEVICE_AUTH_URL: &str = "https://auth.tidal.com/v1/oauth2/device_authorization";
const TOKEN_URL: &str = "https://auth.tidal.com/v1/oauth2/token";
const DEFAULT_ENCODED_CLIENT: &str =
    "NE4zbjZRMXg5NUxMNUs3cDtvS09YZkpXMzcxY1g2eGFaMFB5aGdHTkJkTkxsQlpkNEFLS1lvdWdNamlrPQ==";
const DEFAULT_COUNTRY_CODE: &str = "US";

pub const QUALITY_LOW: &str = "LOW";
pub const QUALITY_HIGH: &str = "HIGH";
pub const QUALITY_LOSSLESS: &str = "LOSSLESS";
pub const QUALITY_HI_RES_LOSSLESS: &str = "HI_RES_LOSSLESS";

#[derive(Debug, Error)]
pub enum Error {
    #[error("token read: {0}")]
    TokenRead(io::Error),
    #[error("token write: {0}")]
    TokenWrite(io::Error),
    #[error("token parse: {0}")]
    TokenParse(serde_json::Error),
    #[error("http request: {0}")]
    Http(reqwest::Error),
    #[error("api error (status {status}): {body}")]
    Api { status: StatusCode, body: String },
    #[error("parse response: {0}")]
    ParseResponse(serde_json::Error),
    #[error("invalid default credentials")]
    InvalidDefaultCredentials,
    #[error("device auth request failed (status {status}): {body}")]
    DeviceAuthRequest { status: StatusCode, body: String },
    #[error("device code expired")]
    DeviceCodeExpired,
    #[error("oauth error: {0} ({1})")]
    OAuth(String, String),
    #[error("unexpected response (status {status}): {body}")]
    UnexpectedResponse { status: StatusCode, body: String },
    #[error("missing stream url in playback manifest")]
    MissingStreamUrl,
    #[error("parse URL: {0}")]
    ParseUrl(url::ParseError),
    #[error("empty URL")]
    EmptyUrl,
    #[error("not a tidal.com URL")]
    NotTidalUrl,
    #[error("unrecognized Tidal URL path: {0}")]
    UnrecognizedUrlPath(String),
    #[error("unsupported Tidal content type: {0}")]
    UnsupportedContentType(String),
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct OAuthToken {
    #[serde(rename = "access_token")]
    pub access_token: String,
    #[serde(rename = "token_type")]
    pub token_type: String,
    #[serde(rename = "refresh_token")]
    pub refresh_token: Option<String>,
    #[serde(rename = "expires_at", default)]
    pub expires_at: Option<i64>,
    #[serde(default)]
    pub scope: String,
}

impl OAuthToken {
    fn auth_header(&self) -> String {
        if self.token_type.trim().is_empty() {
            format!("Bearer {}", self.access_token)
        } else {
            format!("{} {}", self.token_type, self.access_token)
        }
    }

    fn is_expired(&self) -> bool {
        match self.expires_at {
            Some(expires_at) => now_unix() >= expires_at,
            None => false,
        }
    }
}

pub fn cover_url(image_cover: &str) -> String {
    format!("https://resources.tidal.com/images/{image_cover}/320x320.jpg")
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SearchResult {
    pub id: u64,
    pub title: String,
    pub artist: String,
    pub duration: i32,
    pub cover_url: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Track {
    pub id: u64,
    pub title: String,
    pub artist: String,
    pub duration: i32,
    pub cover_url: String,
}

#[derive(Debug, Clone)]
pub struct LyricLine {
    pub timestamp_ms: u64,
    pub text: String,
}

impl LyricLine {
    pub fn parse_lrc(text: &str) -> Vec<LyricLine> {
        parse_lrc(text)
    }
}

#[derive(Debug, Clone)]
pub struct Lyrics {
    pub lines: Vec<LyricLine>,
    pub plain: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TrackStream {
    pub stream_url: String,
    pub duration: i32,
    pub quality: String,
    pub codec: String,
    pub bit_depth: i32,
    pub sample_rate: i32,
}

impl TrackStream {
    pub fn format_audio_info(&self) -> String {
        let codec = match self.codec.as_str() {
            "flac" => "FLAC".to_owned(),
            "mp4a.40.5" => "HE-AAC".to_owned(),
            "mp4a.40.2" => "AAC-LC".to_owned(),
            "mp4a.40.34" => "AAC-LD".to_owned(),
            "mha1" => "MPEG-H".to_owned(),
            other if !other.is_empty() => self.quality.clone(),
            _ => String::new(),
        };

        if self.bit_depth > 0 && self.sample_rate > 0 {
            if self.sample_rate >= 1000 {
                return format!(
                    "{codec} {}bit {}kHz",
                    self.bit_depth,
                    self.sample_rate / 1000
                );
            }
            return format!("{codec} {}bit {}Hz", self.bit_depth, self.sample_rate);
        }

        if !codec.is_empty() {
            return codec;
        }

        self.quality.clone()
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TidalContentType {
    Track,
    Album,
    Playlist,
    Artist,
}

impl fmt::Display for TidalContentType {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            TidalContentType::Track => f.write_str("track"),
            TidalContentType::Album => f.write_str("album"),
            TidalContentType::Playlist => f.write_str("playlist"),
            TidalContentType::Artist => f.write_str("artist"),
        }
    }
}

#[derive(Debug, Clone)]
pub struct Client {
    http: HttpClient,
    token_path: PathBuf,
    token: OAuthToken,
    quality: String,
    country_code: String,
}

impl Client {
    pub async fn new(token_path: impl AsRef<Path>, quality: &str) -> Result<Self, Error> {
        let token_path = token_path.as_ref().to_path_buf();
        let http = HttpClient::builder()
            .timeout(Duration::from_secs(20))
            .build()
            .map_err(Error::Http)?;

        let token = match load_saved_token(&token_path)? {
            Some(tok) => tok,
            None => device_auth(&http, &token_path).await?,
        };

        let mut client = Self {
            http,
            token_path,
            token,
            quality: parse_quality(quality).to_owned(),
            country_code: DEFAULT_COUNTRY_CODE.to_owned(),
        };

        client.ensure_valid_token().await?;
        Ok(client)
    }

    pub async fn search(&mut self, query: &str, limit: usize) -> Result<Vec<SearchResult>, Error> {
        self.ensure_valid_token().await?;

        let url = Url::parse_with_params(
            &format!("{TIDAL_API_BASE}/search/tracks"),
            &[
                ("query", query.to_owned()),
                ("limit", limit.to_string()),
                ("offset", "0".to_owned()),
                ("countryCode", self.country_code.clone()),
            ],
        )
        .map_err(Error::ParseUrl)?;

        let response = self
            .http
            .request(Method::GET, url)
            .header(AUTHORIZATION, self.token.auth_header())
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;
        if !status.is_success() {
            return Err(Error::Api { status, body });
        }

        let parsed: SearchResponse = serde_json::from_str(&body).map_err(Error::ParseResponse)?;
        let results = parsed
            .items
            .into_iter()
            .map(|item| {
                let artist = artist_name(&item);
                let cover = track_cover_url(&item);
                SearchResult {
                    id: item.id,
                    title: item.title,
                    artist,
                    duration: item.duration,
                    cover_url: cover,
                }
            })
            .collect();

        Ok(results)
    }

    pub fn selected_quality(&self) -> &str {
        &self.quality
    }

    pub async fn get_album_tracks(&mut self, album_id: u64) -> Result<Vec<SearchResult>, Error> {
        let path = format!("/albums/{album_id}/items");
        let data: WrappedItemsResponse = self.get_items_endpoint(&path).await?;
        Ok(data
            .items
            .into_iter()
            .map(|it| {
                let artist = artist_name(&it.item);
                let cover = track_cover_url(&it.item);
                SearchResult {
                    id: it.item.id,
                    title: it.item.title,
                    artist,
                    duration: it.item.duration,
                    cover_url: cover,
                }
            })
            .collect())
    }

    pub async fn get_playlist_tracks(
        &mut self,
        playlist_id: &str,
    ) -> Result<Vec<SearchResult>, Error> {
        let path = format!("/playlists/{}/items", urlencoding::encode(playlist_id));
        let data: WrappedItemsResponse = self.get_items_endpoint(&path).await?;
        Ok(data
            .items
            .into_iter()
            .map(|it| {
                let artist = artist_name(&it.item);
                let cover = track_cover_url(&it.item);
                SearchResult {
                    id: it.item.id,
                    title: it.item.title,
                    artist,
                    duration: it.item.duration,
                    cover_url: cover,
                }
            })
            .collect())
    }

    pub async fn get_artist_top_tracks(
        &mut self,
        artist_id: u64,
    ) -> Result<Vec<SearchResult>, Error> {
        let path = format!("/artists/{artist_id}/toptracks");
        let data: SearchResponse = self.get_items_endpoint(&path).await?;
        Ok(data
            .items
            .into_iter()
            .map(|item| {
                let artist = artist_name(&item);
                let cover = track_cover_url(&item);
                SearchResult {
                    id: item.id,
                    title: item.title,
                    artist,
                    duration: item.duration,
                    cover_url: cover,
                }
            })
            .collect())
    }

    pub async fn get_track(&mut self, id: u64) -> Result<Track, Error> {
        self.ensure_valid_token().await?;

        let url = Url::parse_with_params(
            &format!("{TIDAL_API_BASE}/tracks/{id}"),
            &[("countryCode", self.country_code.clone())],
        )
        .map_err(Error::ParseUrl)?;

        let response = self
            .http
            .request(Method::GET, url)
            .header(AUTHORIZATION, self.token.auth_header())
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;
        if !status.is_success() {
            return Err(Error::Api { status, body });
        }

        let item: TidalTrack = serde_json::from_str(&body).map_err(Error::ParseResponse)?;
        let artist = artist_name(&item);
        let cover = track_cover_url(&item);
        Ok(Track {
            id: item.id,
            title: item.title,
            artist,
            duration: item.duration,
            cover_url: cover,
        })
    }

    pub async fn get_lyrics(&mut self, id: u64) -> Result<Lyrics, Error> {
        self.ensure_valid_token().await?;

        let url = Url::parse_with_params(
            &format!("{TIDAL_API_BASE}/tracks/{id}/lyrics"),
            &[("countryCode", self.country_code.clone())],
        )
        .map_err(Error::ParseUrl)?;

        let response = self
            .http
            .request(Method::GET, url)
            .header(AUTHORIZATION, self.token.auth_header())
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;
        if !status.is_success() {
            return Err(Error::Api { status, body });
        }

        #[derive(Deserialize)]
        #[allow(dead_code)]
        struct TidalLyricsResponse {
            #[serde(default)]
            track_id: Option<u64>,
            #[serde(default)]
            lyrics: Option<String>,
            #[serde(default)]
            plain_lyrics: Option<String>,
        }

        let parsed: TidalLyricsResponse =
            serde_json::from_str(&body).map_err(Error::ParseResponse)?;

        let plain = parsed
            .plain_lyrics
            .or_else(|| parsed.lyrics.clone())
            .unwrap_or_default();

        let raw = parsed.lyrics.unwrap_or_default();
        let lines = parse_lrc(&raw);

        Ok(Lyrics { lines, plain })
    }

    pub async fn stream_track(&mut self, id: u64) -> Result<TrackStream, Error> {
        self.ensure_valid_token().await?;

        let quality = if self.quality.is_empty() {
            QUALITY_HI_RES_LOSSLESS
        } else {
            self.quality.as_str()
        };

        let url = Url::parse_with_params(
            &format!("{TIDAL_API_BASE}/tracks/{id}/playbackinfopostpaywall"),
            &[
                ("audioquality", quality.to_owned()),
                ("playbackmode", "STREAM".to_owned()),
                ("assetpresentation", "FULL".to_owned()),
                ("countryCode", self.country_code.clone()),
            ],
        )
        .map_err(Error::ParseUrl)?;

        let response = self
            .http
            .request(Method::GET, url)
            .header(AUTHORIZATION, self.token.auth_header())
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;
        if !status.is_success() {
            return Err(Error::Api { status, body });
        }

        let playback: PlaybackInfoResponse =
            serde_json::from_str(&body).map_err(Error::ParseResponse)?;
        let manifest = decode_manifest(&playback.manifest);
        let parsed_manifest = manifest
            .as_deref()
            .and_then(parse_manifest_json)
            .unwrap_or_default();

        let stream_url = parsed_manifest
            .urls
            .first()
            .cloned()
            .ok_or(Error::MissingStreamUrl)?;

        Ok(TrackStream {
            stream_url,
            duration: playback.duration,
            quality: if playback.audio_quality.is_empty() {
                quality.to_owned()
            } else {
                playback.audio_quality
            },
            codec: parsed_manifest.codecs,
            bit_depth: parsed_manifest.bit_depth,
            sample_rate: parsed_manifest.sample_rate,
        })
    }

    async fn get_items_endpoint<T: for<'de> Deserialize<'de>>(
        &mut self,
        path: &str,
    ) -> Result<T, Error> {
        self.ensure_valid_token().await?;
        let url = Url::parse_with_params(
            &format!("{TIDAL_API_BASE}{path}"),
            &[
                ("limit", "100".to_owned()),
                ("offset", "0".to_owned()),
                ("countryCode", self.country_code.clone()),
            ],
        )
        .map_err(Error::ParseUrl)?;

        let response = self
            .http
            .request(Method::GET, url)
            .header(AUTHORIZATION, self.token.auth_header())
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;
        if !status.is_success() {
            return Err(Error::Api { status, body });
        }

        serde_json::from_str(&body).map_err(Error::ParseResponse)
    }

    async fn ensure_valid_token(&mut self) -> Result<(), Error> {
        if !self.token.is_expired() {
            return Ok(());
        }

        let refresh_token = match self.token.refresh_token.as_deref() {
            Some(refresh) if !refresh.is_empty() => refresh.to_owned(),
            _ => {
                self.token = device_auth(&self.http, &self.token_path).await?;
                return Ok(());
            }
        };

        let (client_id, client_secret) = default_credentials()?;
        let body = format!(
            "grant_type=refresh_token&refresh_token={}&client_id={}&scope=r_usr+w_usr+w_sub",
            refresh_token, client_id
        );
        let response = self
            .http
            .request(Method::POST, TOKEN_URL)
            .header(CONTENT_TYPE, "application/x-www-form-urlencoded")
            .basic_auth(client_id, Some(client_secret))
            .body(body)
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;
        if !status.is_success() {
            self.token = device_auth(&self.http, &self.token_path).await?;
            return Ok(());
        }

        let payload: TokenResponse = serde_json::from_str(&body).map_err(Error::ParseResponse)?;
        self.token = token_from_response(payload, self.token.refresh_token.clone());
        save_token(&self.token_path, &self.token)?;
        Ok(())
    }
}

#[derive(Debug, Deserialize)]
struct SearchResponse {
    #[serde(default)]
    items: Vec<TidalTrack>,
}

#[derive(Debug, Deserialize)]
struct WrappedItemsResponse {
    #[serde(default)]
    items: Vec<WrappedTrack>,
}

#[derive(Debug, Deserialize)]
struct WrappedTrack {
    item: TidalTrack,
}

#[derive(Debug, Deserialize)]
struct TidalArtist {
    name: String,
}

#[derive(Debug, Deserialize)]
struct TidalAlbum {
    cover: Option<String>,
}

#[derive(Debug, Deserialize)]
struct TidalTrack {
    id: u64,
    #[serde(default)]
    title: String,
    #[serde(default, rename = "artistName")]
    artist_name: String,
    #[serde(default)]
    duration: i32,
    #[serde(default)]
    artists: Vec<TidalArtist>,
    #[serde(default)]
    album: Option<TidalAlbum>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct PlaybackInfoResponse {
    #[serde(default)]
    audio_quality: String,
    #[serde(default)]
    manifest: String,
    #[serde(default)]
    duration: i32,
}

#[derive(Debug, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct JsonManifest {
    #[serde(default)]
    urls: Vec<String>,
    #[serde(default)]
    codecs: String,
    #[serde(default)]
    bit_depth: i32,
    #[serde(default)]
    sample_rate: i32,
}

fn track_cover_url(t: &TidalTrack) -> String {
    t.album
        .as_ref()
        .and_then(|a| a.cover.as_deref())
        .map(cover_url)
        .unwrap_or_default()
}

fn artist_name(t: &TidalTrack) -> String {
    if !t.artist_name.is_empty() {
        return t.artist_name.clone();
    }
    t.artists
        .first()
        .map(|artist| artist.name.clone())
        .unwrap_or_default()
}

fn decode_manifest(encoded: &str) -> Option<String> {
    if encoded.is_empty() {
        return None;
    }

    let decoded = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .or_else(|_| base64::engine::general_purpose::URL_SAFE.decode(encoded))
        .ok()?;

    String::from_utf8(decoded).ok()
}

fn parse_manifest_json(raw: &str) -> Option<JsonManifest> {
    serde_json::from_str(raw).ok()
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DeviceAuthResponse {
    device_code: String,
    user_code: String,
    verification_uri_complete: String,
    interval: Option<u64>,
}

#[derive(Debug, Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    refresh_token: Option<String>,
    #[serde(default)]
    expires_in: Option<i64>,
    #[serde(default = "default_bearer")]
    token_type: String,
    #[serde(default)]
    scope: String,
}

fn default_bearer() -> String {
    "Bearer".to_owned()
}

#[derive(Debug, Deserialize)]
struct OAuthErrorResponse {
    error: String,
    #[serde(default)]
    error_description: String,
}

pub fn parse_quality(s: &str) -> &'static str {
    let normalized = s.trim().to_ascii_uppercase();
    match normalized.as_str() {
        QUALITY_LOW => QUALITY_LOW,
        QUALITY_HIGH => QUALITY_HIGH,
        QUALITY_LOSSLESS => QUALITY_LOSSLESS,
        QUALITY_HI_RES_LOSSLESS => QUALITY_HI_RES_LOSSLESS,
        _ => "",
    }
}

pub fn parse_tidal_url(raw_url: &str) -> Result<(TidalContentType, String), Error> {
    if raw_url.is_empty() {
        return Err(Error::EmptyUrl);
    }

    let normalized = if raw_url.contains("://") {
        raw_url.to_owned()
    } else {
        format!("https://{raw_url}")
    };

    let url = Url::parse(&normalized).map_err(Error::ParseUrl)?;
    let host = url.host_str().unwrap_or_default();
    if !host.ends_with("tidal.com") {
        return Err(Error::NotTidalUrl);
    }

    let mut parts = url
        .path()
        .trim_matches('/')
        .split('/')
        .filter(|segment| !segment.is_empty())
        .collect::<Vec<_>>();

    if parts.len() >= 2 && parts[0] == "browse" {
        parts.remove(0);
    }

    if parts.len() < 2 {
        return Err(Error::UnrecognizedUrlPath(url.path().to_owned()));
    }

    let content_type = match parts[0] {
        "track" => TidalContentType::Track,
        "album" => TidalContentType::Album,
        "playlist" => TidalContentType::Playlist,
        "artist" => TidalContentType::Artist,
        other => return Err(Error::UnsupportedContentType(other.to_owned())),
    };

    Ok((content_type, parts[1].to_owned()))
}

fn load_saved_token(path: &Path) -> Result<Option<OAuthToken>, Error> {
    match fs::read_to_string(path) {
        Ok(content) => {
            let token = serde_json::from_str(&content).map_err(Error::TokenParse)?;
            Ok(Some(token))
        }
        Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(err) => Err(Error::TokenRead(err)),
    }
}

fn save_token(path: &Path, token: &OAuthToken) -> Result<(), Error> {
    let data = serde_json::to_string_pretty(token).map_err(Error::TokenParse)?;
    fs::write(path, data).map_err(Error::TokenWrite)
}

fn default_credentials() -> Result<(String, String), Error> {
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(DEFAULT_ENCODED_CLIENT)
        .map_err(|_| Error::InvalidDefaultCredentials)?;
    let decoded = String::from_utf8(decoded).map_err(|_| Error::InvalidDefaultCredentials)?;
    let mut parts = decoded.splitn(2, ';');
    let client_id = parts.next().unwrap_or_default().to_owned();
    let client_secret = parts.next().unwrap_or_default().to_owned();
    if client_id.is_empty() || client_secret.is_empty() {
        return Err(Error::InvalidDefaultCredentials);
    }
    Ok((client_id, client_secret))
}

async fn device_auth(http: &HttpClient, token_path: &Path) -> Result<OAuthToken, Error> {
    let (client_id, client_secret) = default_credentials()?;

    let body = format!("client_id={}&scope=r_usr+w_usr+w_sub", client_id);
    let response = http
        .request(Method::POST, DEVICE_AUTH_URL)
        .header(CONTENT_TYPE, "application/x-www-form-urlencoded")
        .body(body)
        .send()
        .await
        .map_err(Error::Http)?;

    let status = response.status();
    let body = response.text().await.map_err(Error::Http)?;

    if !status.is_success() {
        return Err(Error::DeviceAuthRequest { status, body });
    }

    let auth_resp: DeviceAuthResponse =
        serde_json::from_str(&body).map_err(Error::ParseResponse)?;

    print_device_prompt(&auth_resp);
    sleep(Duration::from_secs(2)).await;

    let token = poll_device_auth(
        http,
        &client_id,
        &client_secret,
        &auth_resp.device_code,
        auth_resp.interval.unwrap_or(2),
    )
    .await?;

    save_token(token_path, &token)?;
    Ok(token)
}

fn print_device_prompt(resp: &DeviceAuthResponse) {
    let mut out = io::stdout();
    let _ = writeln!(out, "\n=== TIDAL DEVICE AUTH ===");
    let _ = writeln!(out, "1. Open: {}", resp.verification_uri_complete);
    let _ = writeln!(out, "2. Enter code: {}", resp.user_code);
    let _ = writeln!(out, "==========================\n");
}

async fn poll_device_auth(
    http: &HttpClient,
    client_id: &str,
    client_secret: &str,
    device_code: &str,
    interval_sec: u64,
) -> Result<OAuthToken, Error> {
    loop {
        let body = format!(
            "client_id={}&device_code={}&grant_type=urn:ietf:params:oauth:grant-type:device_code&scope=r_usr+w_usr+w_sub",
            client_id, device_code
        );
        let response = http
            .request(Method::POST, TOKEN_URL)
            .header(CONTENT_TYPE, "application/x-www-form-urlencoded")
            .basic_auth(client_id, Some(client_secret))
            .body(body)
            .send()
            .await
            .map_err(Error::Http)?;

        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;

        if status.is_success() {
            let token_resp: TokenResponse =
                serde_json::from_str(&body).map_err(Error::ParseResponse)?;
            return Ok(token_from_response(token_resp, None));
        }

        let err_resp: Option<OAuthErrorResponse> = serde_json::from_str(&body).ok();
        if let Some(err_resp) = err_resp {
            match err_resp.error.as_str() {
                "authorization_pending" => {
                    sleep(Duration::from_secs(interval_sec)).await;
                    continue;
                }
                "expired_token" | "invalid_grant" => {
                    return Err(Error::DeviceCodeExpired);
                }
                _ => return Err(Error::OAuth(err_resp.error, err_resp.error_description)),
            }
        }

        return Err(Error::UnexpectedResponse { status, body });
    }
}

fn token_from_response(resp: TokenResponse, fallback_refresh: Option<String>) -> OAuthToken {
    let expires_at = resp.expires_in.map(|seconds| now_unix() + seconds);
    OAuthToken {
        access_token: resp.access_token,
        token_type: resp.token_type,
        refresh_token: resp.refresh_token.or(fallback_refresh),
        expires_at,
        scope: resp.scope,
    }
}

fn parse_lrc(text: &str) -> Vec<LyricLine> {
    let mut lines = Vec::new();
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        if let Some((ts, text)) = parse_lrc_line(line) {
            lines.push(LyricLine {
                timestamp_ms: ts,
                text,
            });
        }
    }
    lines.sort_by_key(|l| l.timestamp_ms);
    lines
}

fn parse_lrc_line(line: &str) -> Option<(u64, String)> {
    let line = line.trim_start();
    if !line.starts_with('[') {
        return None;
    }
    let close = line.find(']')?;
    let time_str = &line[1..close];
    let text = line[close + 1..].trim().to_owned();
    if text.is_empty() {
        return None;
    }
    let ts = parse_lrc_timestamp(time_str)?;
    Some((ts, text))
}

fn parse_lrc_timestamp(s: &str) -> Option<u64> {
    // [mm:ss.xx] or [mm:ss.xxx] or [mm:ss:xx]
    let s = s.trim();
    let colon = s.find(':')?;
    let minutes: u64 = s[..colon].parse().ok()?;
    let rest = &s[colon + 1..];
    let dot = rest.find(['.', ':']).unwrap_or(rest.len());
    let seconds: u64 = rest[..dot].parse().ok()?;
    let millis = if dot < rest.len() {
        let frac = &rest[dot + 1..];
        let padded = format!("{:<03}", frac);
        padded[..3].parse::<u64>().unwrap_or(0)
    } else {
        0
    };
    Some(minutes * 60_000 + seconds * 1_000 + millis)
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or(Duration::from_secs(0))
        .as_secs() as i64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn format_audio_info() {
        let cases = vec![
            (
                TrackStream {
                    stream_url: String::new(),
                    duration: 0,
                    quality: "HI_RES_LOSSLESS".to_owned(),
                    codec: "flac".to_owned(),
                    bit_depth: 24,
                    sample_rate: 48000,
                },
                "FLAC 24bit 48kHz",
            ),
            (
                TrackStream {
                    stream_url: String::new(),
                    duration: 0,
                    quality: "HIGH".to_owned(),
                    codec: "mp4a.40.2".to_owned(),
                    bit_depth: 16,
                    sample_rate: 44100,
                },
                "AAC-LC 16bit 44kHz",
            ),
            (
                TrackStream {
                    stream_url: String::new(),
                    duration: 0,
                    quality: "LOSSLESS".to_owned(),
                    codec: "unknown".to_owned(),
                    bit_depth: 0,
                    sample_rate: 0,
                },
                "LOSSLESS",
            ),
            (
                TrackStream {
                    stream_url: String::new(),
                    duration: 0,
                    quality: "LOW".to_owned(),
                    codec: "flac".to_owned(),
                    bit_depth: 16,
                    sample_rate: 800,
                },
                "FLAC 16bit 800Hz",
            ),
        ];

        for (stream, want) in cases {
            assert_eq!(stream.format_audio_info(), want);
        }
    }

    #[test]
    fn parse_quality_tests() {
        assert_eq!(parse_quality("LOW"), "LOW");
        assert_eq!(parse_quality("low"), "LOW");
        assert_eq!(parse_quality("  high  "), "HIGH");
        assert_eq!(parse_quality("INVALID"), "");
    }

    #[test]
    fn parse_tidal_url_tests() {
        let (kind, id) = parse_tidal_url("https://tidal.com/track/123").unwrap();
        assert_eq!(kind, TidalContentType::Track);
        assert_eq!(id, "123");

        let (kind, id) = parse_tidal_url("tidal.com/browse/playlist/abc").unwrap();
        assert_eq!(kind, TidalContentType::Playlist);
        assert_eq!(id, "abc");

        assert!(matches!(parse_tidal_url(""), Err(Error::EmptyUrl)));
        assert!(matches!(
            parse_tidal_url("https://example.com/track/1"),
            Err(Error::NotTidalUrl)
        ));
    }

    #[test]
    fn artist_name_fallback() {
        let track = TidalTrack {
            id: 1,
            title: String::new(),
            artist_name: "Daft Punk".to_owned(),
            duration: 0,
            artists: vec![TidalArtist {
                name: "Other".to_owned(),
            }],
            album: None,
        };
        assert_eq!(artist_name(&track), "Daft Punk");

        let track = TidalTrack {
            id: 1,
            title: String::new(),
            artist_name: String::new(),
            duration: 0,
            artists: vec![TidalArtist {
                name: "Daft Punk".to_owned(),
            }],
            album: None,
        };
        assert_eq!(artist_name(&track), "Daft Punk");
    }

    #[test]
    fn decode_and_parse_manifest() {
        let manifest_json = r#"{"codecs":"flac","urls":["https://stream.example.com/audio.flac"],"bitDepth":24,"sampleRate":48000}"#;
        let manifest_b64 = base64::engine::general_purpose::STANDARD.encode(manifest_json);
        let decoded = decode_manifest(&manifest_b64).unwrap();
        let parsed = parse_manifest_json(&decoded).unwrap();
        assert_eq!(parsed.codecs, "flac");
        assert_eq!(parsed.bit_depth, 24);
        assert_eq!(parsed.sample_rate, 48000);
        assert_eq!(parsed.urls.len(), 1);
    }
}

use crate::commands::{Command, parse_command};
use crate::queue::{Queue, Track};
use chatto::{Client as ChattoClient, RoomTimelineEvent};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tidal::Client as TidalClient;
use tokio::sync::Mutex;
use tracing::{error, info, warn};

#[derive(Debug)]
pub struct BotConfig {
    pub rooms: Vec<String>,
    pub poll_interval: Duration,
    pub bot_name: String,
    pub volume: u8,
}

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("chatto error: {0}")]
    Chatto(#[from] chatto::Error),
    #[error("tidal error: {0}")]
    Tidal(#[from] tidal::Error),
}

#[derive(Debug, Clone)]
struct CurrentTrack {
    track: Track,
    audio_info: String,
}

#[derive(Debug, Default)]
struct RoomState {
    queue: Queue,
    current: Option<CurrentTrack>,
}

pub struct Bot {
    cfg: BotConfig,
    chatto: ChattoClient,
    tidal: Arc<Mutex<TidalClient>>,
    rooms: Arc<Mutex<HashMap<String, RoomState>>>,
    cursors: Arc<Mutex<HashMap<String, String>>>,
    seen_events: Arc<Mutex<HashSet<String>>>,
    volume: Arc<Mutex<f64>>,
    shutdown: Arc<AtomicBool>,
}

impl Bot {
    pub fn new(cfg: BotConfig, chatto: ChattoClient, tidal: TidalClient) -> Self {
        let rooms = cfg
            .rooms
            .iter()
            .map(|room_id| (room_id.clone(), RoomState::default()))
            .collect();

        Self {
            volume: Arc::new(Mutex::new(f64::from(cfg.volume) / 100.0)),
            cfg,
            chatto,
            tidal: Arc::new(Mutex::new(tidal)),
            rooms: Arc::new(Mutex::new(rooms)),
            cursors: Arc::new(Mutex::new(HashMap::new())),
            seen_events: Arc::new(Mutex::new(HashSet::new())),
            shutdown: Arc::new(AtomicBool::new(false)),
        }
    }

    pub fn shutdown(&self) {
        self.shutdown.store(true, Ordering::SeqCst);
    }

    pub async fn run(&self) -> Result<(), Error> {
        self.set_presence().await;

        let mut poll_tick = tokio::time::interval(self.cfg.poll_interval);
        let mut presence_tick = tokio::time::interval(Duration::from_secs(45));

        info!(rooms = ?self.cfg.rooms, "bot runtime started");

        loop {
            if self.shutdown.load(Ordering::SeqCst) {
                info!("bot runtime shutdown requested");
                return Ok(());
            }

            tokio::select! {
                _ = poll_tick.tick() => {
                    self.poll_all_rooms().await?;
                    self.auto_prepare_playback().await?;
                }
                _ = presence_tick.tick() => {
                    self.set_presence().await;
                }
            }
        }
    }

    async fn set_presence(&self) {
        if let Err(err) = self
            .chatto
            .update_presence("PRESENCE_STATUS_ONLINE", true)
            .await
        {
            warn!(error = %err, "failed to set online presence");
        }

        if let Err(err) = self
            .chatto
            .update_custom_status("🎧", "Providing high-res songs")
            .await
        {
            warn!(error = %err, "failed to set custom status");
        }
    }

    async fn poll_all_rooms(&self) -> Result<(), Error> {
        for room_id in &self.cfg.rooms {
            self.poll_room(room_id).await?;
        }
        Ok(())
    }

    async fn poll_room(&self, room_id: &str) -> Result<(), Error> {
        let after = {
            let cursors = self.cursors.lock().await;
            cursors.get(room_id).cloned().unwrap_or_default()
        };

        match self.chatto.get_room_events(room_id, &after, 50).await {
            Ok(resp) => {
                if let Some(page) = resp.page {
                    {
                        let mut cursors = self.cursors.lock().await;
                        cursors.insert(room_id.to_owned(), page.end_cursor.clone());
                    }

                    for event in page.events {
                        self.process_event(room_id, event).await?;
                    }
                }
                Ok(())
            }
            Err(err)
                if chatto::is_not_member_error(&err)
                    || chatto::is_permission_denied_error(&err) =>
            {
                warn!(room = room_id, error = %err, "bot not a member or lacks permission");
                Ok(())
            }
            Err(err) => Err(Error::Chatto(err)),
        }
    }

    async fn process_event(&self, room_id: &str, event: RoomTimelineEvent) -> Result<(), Error> {
        {
            let mut seen = self.seen_events.lock().await;
            if !seen.insert(event.id.clone()) {
                return Ok(());
            }
        }

        let Some(message_posted) = event.message_posted else {
            return Ok(());
        };
        let Some(body) = message_posted.message.body else {
            return Ok(());
        };
        if body.trim().is_empty() {
            return Ok(());
        }

        let Some(parsed) = parse_command(&body, &self.cfg.bot_name) else {
            return Ok(());
        };

        match parsed.command {
            Command::Help => {
                self.send_message(room_id, help_message()).await;
            }
            Command::Play | Command::Queue => {
                self.cmd_queue(room_id, &message_posted.message.actor_id, &parsed.args)
                    .await?;
            }
            Command::NowPlaying => {
                self.cmd_now_playing(room_id).await;
            }
            Command::Skip => {
                self.cmd_skip(room_id).await;
            }
            Command::Stop => {
                self.cmd_stop(room_id).await;
            }
            Command::Volume => {
                self.cmd_volume(room_id, &parsed.args).await;
            }
            Command::Test => {
                self.send_message(
                    room_id,
                    "LiveKit test command kept; Rust audio publisher wiring is next.",
                )
                .await;
            }
        }

        Ok(())
    }

    async fn cmd_queue(&self, room_id: &str, actor_id: &str, query: &str) -> Result<(), Error> {
        if query.trim().is_empty() {
            self.print_queue(room_id).await;
            return Ok(());
        }

        let mut tidal = self.tidal.lock().await;
        let results = tidal.search(query, 5).await?;
        drop(tidal);

        if results.is_empty() {
            self.send_message(room_id, &format!("No results for **{}**", query))
                .await;
            return Ok(());
        }

        let first = &results[0];
        let track = Track {
            tid: first.id,
            title: first.title.clone(),
            artist: first.artist.clone(),
            duration: first.duration,
            requestor: actor_id.to_owned(),
        };

        let pos = {
            let mut rooms = self.rooms.lock().await;
            let rs = rooms.entry(room_id.to_owned()).or_default();
            rs.queue.add(track.clone());
            rs.queue.total_len()
        };

        self.send_message(
            room_id,
            &format!(
                "Added **{}** · *{}* (`{}`) at #{}",
                track.title,
                track.artist,
                format_duration(track.duration),
                pos
            ),
        )
        .await;

        Ok(())
    }

    async fn print_queue(&self, room_id: &str) {
        let message = {
            let rooms = self.rooms.lock().await;
            let Some(rs) = rooms.get(room_id) else {
                return;
            };

            let list = rs.queue.list();
            if rs.current.is_none() && list.is_empty() {
                "Queue is empty.".to_owned()
            } else {
                let mut body = String::new();
                if let Some(current) = &rs.current {
                    body.push_str(&format!(
                        "Now: **{}** · *{}* (`{}`)\n",
                        current.track.title,
                        current.track.artist,
                        format_duration(current.track.duration)
                    ));
                }
                for (idx, track) in list.iter().enumerate() {
                    body.push_str(&format!(
                        "`{}.` **{}** · *{}* (`{}`)\n",
                        idx + 1,
                        track.title,
                        track.artist,
                        format_duration(track.duration)
                    ));
                }
                body
            }
        };

        self.send_message(room_id, &message).await;
    }

    async fn cmd_now_playing(&self, room_id: &str) {
        let message = {
            let rooms = self.rooms.lock().await;
            let Some(rs) = rooms.get(room_id) else {
                return;
            };

            match &rs.current {
                Some(current) => format!(
                    "Now playing **{}** · *{}* (`{}`) · {}",
                    current.track.title,
                    current.track.artist,
                    format_duration(current.track.duration),
                    current.audio_info
                ),
                None => "Nothing is currently prepared for playback.".to_owned(),
            }
        };

        self.send_message(room_id, &message).await;
    }

    async fn cmd_skip(&self, room_id: &str) {
        let skipped = {
            let mut rooms = self.rooms.lock().await;
            let Some(rs) = rooms.get_mut(room_id) else {
                return;
            };
            rs.current.take()
        };

        match skipped {
            Some(current) => {
                self.send_message(room_id, &format!("Skipped **{}**", current.track.title))
                    .await;
            }
            None => {
                self.send_message(room_id, "Nothing to skip.").await;
            }
        }
    }

    async fn cmd_stop(&self, room_id: &str) {
        let removed = {
            let mut rooms = self.rooms.lock().await;
            let Some(rs) = rooms.get_mut(room_id) else {
                return;
            };

            let count = rs.queue.len();
            rs.queue.clear();
            rs.current = None;
            count
        };

        self.send_message(
            room_id,
            &format!("Stopped, removed {removed} queued track(s)."),
        )
        .await;
    }

    async fn cmd_volume(&self, room_id: &str, args: &str) {
        if args.trim().is_empty() {
            let current = *self.volume.lock().await;
            self.send_message(
                room_id,
                &format!("Current volume: `{:.0}%`", current * 100.0),
            )
            .await;
            return;
        }

        let Ok(pct) = args.trim().parse::<u16>() else {
            self.send_message(room_id, "Usage: `volume <0-200>`").await;
            return;
        };

        if pct > 200 {
            self.send_message(room_id, "Usage: `volume <0-200>`").await;
            return;
        }

        let mut volume = self.volume.lock().await;
        *volume = f64::from(pct) / 100.0;
        self.send_message(room_id, &format!("Set volume to `{pct}%`"))
            .await;
    }

    async fn auto_prepare_playback(&self) -> Result<(), Error> {
        for room_id in &self.cfg.rooms {
            let next_track = {
                let mut rooms = self.rooms.lock().await;
                let Some(rs) = rooms.get_mut(room_id) else {
                    continue;
                };
                if rs.current.is_some() {
                    None
                } else {
                    rs.queue.next()
                }
            };

            let Some(track) = next_track else {
                continue;
            };

            let stream = {
                let mut tidal = self.tidal.lock().await;
                tidal.stream_track(track.tid).await?
            };

            {
                let mut rooms = self.rooms.lock().await;
                if let Some(rs) = rooms.get_mut(room_id) {
                    rs.current = Some(CurrentTrack {
                        track: track.clone(),
                        audio_info: stream.format_audio_info(),
                    });
                }
            }

            self.send_message(
                room_id,
                &format!(
                    "Prepared **{}** · *{}* (`{}`) · {}\nStream: {}",
                    track.title,
                    track.artist,
                    format_duration(track.duration),
                    stream.format_audio_info(),
                    stream.stream_url
                ),
            )
            .await;
        }

        Ok(())
    }

    async fn send_message(&self, room_id: &str, text: &str) {
        if let Err(err) = self.chatto.create_message(room_id, text).await {
            error!(room = room_id, error = %err, "failed to send message");
        }
    }
}

fn format_duration(seconds: i32) -> String {
    let minutes = seconds / 60;
    let seconds = seconds % 60;
    format!("{minutes}:{seconds:02}")
}

fn help_message() -> &'static str {
    "`play <track>`\n`queue <track>`\n`queue`\n`skip`\n`stop`\n`nowplaying`\n`volume <0-200>`\n`test`\n`help`"
}

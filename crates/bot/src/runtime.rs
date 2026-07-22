use crate::commands::{Command, parse_command};
use crate::queue::{Queue, Track};
use chatto::{Client as ChattoClient, RoomTimelineEvent};
use chrono::{DateTime, NaiveDateTime, Utc};
use livekit_audio::Player as LivekitPlayer;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tidal::Client as TidalClient;
use tokio::sync::Mutex;
use tokio::task::JoinHandle;
use tracing::{error, info, warn};

#[derive(Debug)]
pub struct BotConfig {
    pub rooms: Vec<String>,
    pub poll_interval: Duration,
    pub bot_name: String,
    pub volume: u8,
    pub sample_rate: u32,
}

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("chatto error: {0}")]
    Chatto(#[from] chatto::Error),
    #[error("tidal error: {0}")]
    Tidal(#[from] tidal::Error),
    #[error("livekit audio error: {0}")]
    LivekitAudio(#[from] livekit_audio::Error),
}

#[derive(Debug, Clone)]
struct CurrentTrack {
    track: Track,
    audio_info: String,
    stream_url: String,
}

#[derive(Debug, Default)]
struct RoomState {
    queue: Queue,
    current: Option<CurrentTrack>,
    playback_task: Option<JoinHandle<PlaybackTaskResult>>,
    playback_cancel: Option<Arc<AtomicBool>>,
    cursor: String,
}

#[derive(Debug)]
enum PlaybackTaskResult {
    Finished,
    VoiceRequired,
    Cancelled,
    Error(String),
}

pub struct Bot {
    cfg: BotConfig,
    livekit_url: String,
    chatto: ChattoClient,
    tidal: Arc<Mutex<TidalClient>>,
    rooms: Arc<Mutex<HashMap<String, RoomState>>>,
    volume: Arc<Mutex<f64>>,
    shutdown: Arc<AtomicBool>,
    started_at: DateTime<Utc>,
    seen_events: Arc<Mutex<HashSet<String>>>,
    bot_user_id: Arc<Mutex<Option<String>>>,
}

impl Bot {
    pub fn new(
        cfg: BotConfig,
        livekit_url: String,
        chatto: ChattoClient,
        tidal: TidalClient,
    ) -> Self {
        let rooms = cfg
            .rooms
            .iter()
            .map(|room_id| (room_id.clone(), RoomState::default()))
            .collect();

        Self {
            volume: Arc::new(Mutex::new(f64::from(cfg.volume) / 100.0)),
            cfg,
            livekit_url,
            chatto,
            tidal: Arc::new(Mutex::new(tidal)),
            rooms: Arc::new(Mutex::new(rooms)),
            shutdown: Arc::new(AtomicBool::new(false)),
            started_at: Utc::now(),
            seen_events: Arc::new(Mutex::new(HashSet::new())),
            bot_user_id: Arc::new(Mutex::new(None)),
        }
    }

    pub fn shutdown(&self) {
        self.shutdown.store(true, Ordering::SeqCst);
    }

    pub async fn run(&self) -> Result<(), Error> {
        self.set_presence().await;

        match self.chatto.get_viewer().await {
            Ok(id) => {
                let mut uid = self.bot_user_id.lock().await;
                *uid = Some(id);
            }
            Err(err) => {
                warn!(error = %err, "could not get bot user ID, own messages will not be filtered");
            }
        }

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
                    self.reap_playback_tasks().await;
                    self.poll_all_rooms().await;
                    self.auto_prepare_playback().await;
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

    async fn poll_all_rooms(&self) {
        for room_id in &self.cfg.rooms {
            if let Err(err) = self.poll_room(room_id).await {
                error!(room = room_id, error = %err, "room poll failed");
            }
        }
    }

    async fn poll_room(&self, room_id: &str) -> Result<(), Error> {
        let after = {
            let rooms = self.rooms.lock().await;
            rooms
                .get(room_id)
                .map(|rs| rs.cursor.clone())
                .unwrap_or_default()
        };

        match self.chatto.get_room_events(room_id, &after, 50).await {
            Ok(resp) => {
                if let Some(page) = resp.page {
                    {
                        let mut rooms = self.rooms.lock().await;
                        if let Some(rs) = rooms.get_mut(room_id) {
                            rs.cursor = page.end_cursor;
                        }
                    }

                    let events = page.events;
                    for event in events {
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
        let Some(message_posted) = event.message_posted else {
            return Ok(());
        };

        {
            let mut seen = self.seen_events.lock().await;
            if !seen.insert(event.id.clone()) {
                return Ok(());
            }
        }

        {
            let uid = self.bot_user_id.lock().await;
            if uid.as_deref() == Some(&message_posted.message.actor_id) {
                return Ok(());
            }
        }

        if let Some(event_time) = parse_event_time(&event.created_at)
            && event_time < self.started_at
        {
            return Ok(());
        }

        let Some(body) = message_posted.message.body else {
            return Ok(());
        };
        if body.trim().is_empty() {
            return Ok(());
        }

        let Some(parsed) = parse_command(&body, &self.cfg.bot_name) else {
            return Ok(());
        };

        info!(
            cmd = ?parsed.command,
            args = parsed.args,
            actor = message_posted.message.actor_id,
            "processing command"
        );

        match parsed.command {
            Command::Help => {
                self.send_message(room_id, &help_message()).await;
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
                self.cmd_test(room_id).await;
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
            self.send_message(
                room_id,
                &card("Not Found", &format!("No results for: {}", query)),
            )
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
            &card(
                "Added",
                &format!(
                    "{} · {} ({})\nPosition: #{}",
                    track.title,
                    track.artist,
                    format_duration(track.duration),
                    pos
                ),
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
                card("Queue", "Queue is empty.")
            } else {
                let mut body = String::new();
                if let Some(current) = &rs.current {
                    body.push_str(&format!(
                        "{} · {} ({})\n\n",
                        current.track.title,
                        current.track.artist,
                        format_duration(current.track.duration)
                    ));
                }
                for (idx, track) in list.iter().enumerate() {
                    body.push_str(&format!(
                        "{}. {} · {} ({})\n",
                        idx + 1,
                        track.title,
                        track.artist,
                        format_duration(track.duration)
                    ));
                }
                card("Queue", &body)
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
                Some(current) => card(
                    "Now Playing",
                    &format!(
                        "{} · {} ({})\n{}",
                        current.track.title,
                        current.track.artist,
                        format_duration(current.track.duration),
                        current.audio_info
                    ),
                ),
                None => card("Now Playing", "Nothing currently prepared."),
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
            if let Some(cancel) = rs.playback_cancel.take() {
                cancel.store(true, Ordering::SeqCst);
            }
            rs.current.take()
        };

        match skipped {
            Some(current) => {
                self.send_message(room_id, &card("Skipped", &current.track.title))
                    .await;
            }
            None => {
                self.send_message(room_id, &card("Skipped", "Nothing playing."))
                    .await;
            }
        }
    }

    async fn cmd_stop(&self, room_id: &str) {
        let removed = {
            let mut rooms = self.rooms.lock().await;
            let Some(rs) = rooms.get_mut(room_id) else {
                return;
            };

            if let Some(cancel) = rs.playback_cancel.take() {
                cancel.store(true, Ordering::SeqCst);
            }

            let count = rs.queue.len();
            rs.queue.clear();
            rs.current = None;
            count
        };

        self.send_message(
            room_id,
            &card("Stopped", &format!("Removed {removed} queued track(s).")),
        )
        .await;
    }

    async fn cmd_volume(&self, room_id: &str, args: &str) {
        if args.trim().is_empty() {
            let current = *self.volume.lock().await;
            self.send_message(
                room_id,
                &card("Volume", &format!("Current: {:.0}%", current * 100.0)),
            )
            .await;
            return;
        }

        let Ok(pct) = args.trim().parse::<u16>() else {
            self.send_message(room_id, &card("Volume", "Usage: volume <0-200>"))
                .await;
            return;
        };

        if pct > 200 {
            self.send_message(room_id, &card("Volume", "Usage: volume <0-200>"))
                .await;
            return;
        }

        let mut volume = self.volume.lock().await;
        *volume = f64::from(pct) / 100.0;
        self.send_message(room_id, &card("Volume", &format!("Set to {}%", pct)))
            .await;
    }

    async fn auto_prepare_playback(&self) {
        for room_id in &self.cfg.rooms {
            let next_track = {
                let mut rooms = self.rooms.lock().await;
                let Some(rs) = rooms.get_mut(room_id) else {
                    continue;
                };
                if rs.current.is_some() || rs.playback_task.is_some() {
                    None
                } else {
                    rs.queue.dequeue()
                }
            };

            let Some(track) = next_track else {
                continue;
            };

            let stream = {
                let mut tidal = self.tidal.lock().await;
                match tidal.stream_track(track.tid).await {
                    Ok(stream) => stream,
                    Err(err) => {
                        error!(room = room_id, track_id = track.tid, error = %err, "failed to resolve stream");
                        self.send_message(room_id, &card("Stream Error", &err.to_string()))
                            .await;
                        continue;
                    }
                }
            };

            {
                let mut rooms = self.rooms.lock().await;
                if let Some(rs) = rooms.get_mut(room_id) {
                    rs.current = Some(CurrentTrack {
                        track: track.clone(),
                        audio_info: stream.format_audio_info(),
                        stream_url: stream.stream_url.clone(),
                    });
                }
            }

            self.start_playback_task(room_id).await;
        }
    }

    async fn send_message(&self, room_id: &str, text: &str) {
        if let Err(err) = self.chatto.create_message(room_id, text).await {
            error!(room = room_id, error = %err, "failed to send message");
        }
    }

    async fn cmd_test(&self, room_id: &str) {
        self.send_message(room_id, &card("Test", "Publishing 10s of silence..."))
            .await;

        let token = match self.chatto.get_call_token(room_id).await {
            Ok(token) => token,
            Err(err) => {
                self.send_message(
                    room_id,
                    &card("Test Error", &format!("Get call token: {err}")),
                )
                .await;
                return;
            }
        };

        let mut player = match LivekitPlayer::new(livekit_audio::Config {
            url: self.livekit_url.clone(),
            token: token.token,
            room: room_id.to_owned(),
            sample_rate: self.cfg.sample_rate,
        })
        .await
        {
            Ok(player) => player,
            Err(err) => {
                self.send_message(
                    room_id,
                    &card("Test Error", &format!("Create player: {err}")),
                )
                .await;
                return;
            }
        };

        match player.play_silence_only(Duration::from_secs(10)).await {
            Ok(()) => {
                self.send_message(room_id, &card("Test", "10s silence published OK."))
                    .await;
            }
            Err(err) => {
                self.send_message(room_id, &card("Test Failed", &err.to_string()))
                    .await;
            }
        }

        player.disconnect().await;
    }

    async fn start_playback_task(&self, room_id: &str) {
        let (stream_url, already_running) = {
            let rooms = self.rooms.lock().await;
            let Some(rs) = rooms.get(room_id) else {
                return;
            };
            let Some(current) = rs.current.as_ref() else {
                return;
            };
            (current.stream_url.clone(), rs.playback_task.is_some())
        };

        if already_running {
            return;
        }

        let room_id_owned = room_id.to_owned();
        let livekit_url = self.livekit_url.clone();
        let sample_rate = self.cfg.sample_rate;
        let chatto = self.chatto.clone();
        let volume = *self.volume.lock().await as f32;
        let cancel = Arc::new(AtomicBool::new(false));
        let cancel_for_task = Arc::clone(&cancel);

        let task = tokio::spawn(async move {
            let joined = match chatto.join_call(&room_id_owned).await {
                Ok(joined) => joined,
                Err(err) => return PlaybackTaskResult::Error(format!("join call failed: {err}")),
            };

            if !joined {
                return PlaybackTaskResult::VoiceRequired;
            }

            let token = match chatto.get_call_token(&room_id_owned).await {
                Ok(token) => token,
                Err(err) => {
                    return PlaybackTaskResult::Error(format!("get call token failed: {err}"));
                }
            };

            let mut player = match LivekitPlayer::new(livekit_audio::Config {
                url: livekit_url,
                token: token.token,
                room: room_id_owned,
                sample_rate,
            })
            .await
            {
                Ok(player) => player,
                Err(err) => {
                    return PlaybackTaskResult::Error(format!("livekit init failed: {err}"));
                }
            };

            player.set_volume(volume);
            let playback_result = player.play_url_until(&stream_url, cancel_for_task).await;
            player.disconnect().await;

            match playback_result {
                Ok(()) => PlaybackTaskResult::Finished,
                Err(livekit_audio::Error::Cancelled) => PlaybackTaskResult::Cancelled,
                Err(err) => PlaybackTaskResult::Error(format!("playback failed: {err}")),
            }
        });

        let mut rooms = self.rooms.lock().await;
        if let Some(rs) = rooms.get_mut(room_id) {
            rs.playback_task = Some(task);
            rs.playback_cancel = Some(cancel);
        }
    }

    async fn reap_playback_tasks(&self) {
        for room_id in &self.cfg.rooms {
            let finished_handle = {
                let mut rooms = self.rooms.lock().await;
                let Some(rs) = rooms.get_mut(room_id) else {
                    continue;
                };

                if rs
                    .playback_task
                    .as_ref()
                    .is_some_and(JoinHandle::is_finished)
                {
                    rs.playback_cancel = None;
                    rs.playback_task.take()
                } else {
                    None
                }
            };

            let Some(handle) = finished_handle else {
                continue;
            };

            let task_result = match handle.await {
                Ok(result) => result,
                Err(err) => PlaybackTaskResult::Error(format!("playback task join error: {err}")),
            };

            let should_clear_current = !matches!(task_result, PlaybackTaskResult::Cancelled);
            if should_clear_current {
                let mut rooms = self.rooms.lock().await;
                if let Some(rs) = rooms.get_mut(room_id) {
                    rs.current = None;
                }
            }

            match task_result {
                PlaybackTaskResult::Finished | PlaybackTaskResult::Cancelled => {}
                PlaybackTaskResult::VoiceRequired => {
                    self.send_message(
                        room_id,
                        &card(
                            "Voice Required",
                            "Join a voice channel first, then use `play`.",
                        ),
                    )
                    .await;
                }
                PlaybackTaskResult::Error(err) => {
                    error!(room = room_id, error = %err, "playback task error");
                    self.send_message(room_id, &card("Playback Error", &err.to_string()))
                        .await;
                }
            }
        }
    }
}

fn parse_event_time(s: &str) -> Option<DateTime<Utc>> {
    if let Ok(dt) = DateTime::parse_from_rfc3339(s) {
        return Some(dt.with_timezone(&Utc));
    }
    let naive = NaiveDateTime::parse_from_str(s, "%Y-%m-%dT%H:%M:%S%.f").ok()?;
    Some(DateTime::from_naive_utc_and_offset(naive, Utc))
}

fn card(title: &str, body: &str) -> String {
    const WIDTH: usize = 52;

    let mut out = String::with_capacity(WIDTH * (body.lines().count() + 3));

    let title_len = title.chars().count();
    let dashes = WIDTH.saturating_sub(title_len + 5);
    out.push_str("┌─ ");
    out.push_str(title);
    out.push(' ');
    for _ in 0..dashes {
        out.push('─');
    }
    out.push_str("┐\n");

    for line in body.lines() {
        out.push_str("│  ");
        out.push_str(line);
        out.push('\n');
    }

    out.push('└');
    for _ in 0..(WIDTH - 2) {
        out.push('─');
    }
    out.push('┘');
    out
}

fn format_duration(seconds: i32) -> String {
    let minutes = seconds / 60;
    let seconds = seconds % 60;
    format!("{minutes}:{seconds:02}")
}

fn help_message() -> String {
    card(
        "Commands",
        "play <track>\nqueue <track>\nqueue\nskip\nstop\nnowplaying\nvolume <0-200>\ntest\nhelp",
    )
}

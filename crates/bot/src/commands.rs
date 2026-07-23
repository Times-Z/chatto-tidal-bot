#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Command {
    Play,
    Queue,
    Skip,
    Stop,
    NowPlaying,
    Volume,
    Mute,
    Test,
    Help,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedCommand {
    pub command: Command,
    pub args: String,
}

pub fn parse_command(body: &str, bot_name: &str) -> Option<ParsedCommand> {
    let mut text = body.trim();
    if text.is_empty() {
        return None;
    }

    let (stripped, _) = strip_mention(text, bot_name);
    text = stripped.trim();
    text = text.strip_prefix('/').unwrap_or(text);

    if text.is_empty() {
        return None;
    }

    let mut parts = text.splitn(2, char::is_whitespace);
    let raw_command = parts.next()?.to_ascii_lowercase();
    let args = parts.next().map(str::trim).unwrap_or_default().to_owned();

    let command = match raw_command.as_str() {
        "play" => Command::Play,
        "queue" => Command::Queue,
        "skip" => Command::Skip,
        "stop" => Command::Stop,
        "nowplaying" => Command::NowPlaying,
        "volume" => Command::Volume,
        "mute" | "unmute" => Command::Mute,
        "test" => Command::Test,
        "help" => Command::Help,
        _ => return None,
    };

    Some(ParsedCommand { command, args })
}

fn strip_mention<'a>(body: &'a str, bot_name: &str) -> (&'a str, bool) {
    if bot_name.is_empty() {
        return (body, false);
    }

    let prefix = format!("@{bot_name}");
    if body
        .get(..prefix.len())
        .is_some_and(|head| head.eq_ignore_ascii_case(prefix.as_str()))
    {
        (&body[prefix.len()..], true)
    } else {
        (body, false)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_plain_command() {
        let cmd = parse_command("play Around The World", "tidal.bot").unwrap();
        assert_eq!(cmd.command, Command::Play);
        assert_eq!(cmd.args, "Around The World");
    }

    #[test]
    fn parse_slash_command() {
        let cmd = parse_command("/queue Daft Punk", "tidal.bot").unwrap();
        assert_eq!(cmd.command, Command::Queue);
        assert_eq!(cmd.args, "Daft Punk");
    }

    #[test]
    fn parse_mention_and_slash() {
        let cmd = parse_command("@tidal.bot /skip", "tidal.bot").unwrap();
        assert_eq!(cmd.command, Command::Skip);
        assert_eq!(cmd.args, "");
    }

    #[test]
    fn parse_mention_and_plain() {
        let cmd = parse_command("@tidal.bot nowplaying", "tidal.bot").unwrap();
        assert_eq!(cmd.command, Command::NowPlaying);
        assert_eq!(cmd.args, "");
    }

    #[test]
    fn parse_case_insensitive_command() {
        let cmd = parse_command("VoLuMe 120", "tidal.bot").unwrap();
        assert_eq!(cmd.command, Command::Volume);
        assert_eq!(cmd.args, "120");
    }

    #[test]
    fn parse_empty_body() {
        assert!(parse_command("   ", "tidal.bot").is_none());
    }

    #[test]
    fn parse_unknown_command() {
        assert!(parse_command("dance now", "tidal.bot").is_none());
    }

    #[test]
    fn parse_only_mention() {
        assert!(parse_command("@tidal.bot", "tidal.bot").is_none());
    }
}

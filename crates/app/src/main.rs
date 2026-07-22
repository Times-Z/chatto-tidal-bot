use anyhow::{Context, Result};
use chatto::Client as ChattoClient;
use config::AppConfig;
use std::env;
use tracing::{error, info};

#[tokio::main]
async fn main() {
    if let Err(err) = run().await {
        error!(error = %err, "fatal error");
        std::process::exit(1);
    }
}

async fn run() -> Result<()> {
    init_tracing();

    let config_path = env::args()
        .nth(1)
        .unwrap_or_else(|| "config.json".to_owned());

    let cfg = AppConfig::load_from_path(&config_path)
        .with_context(|| format!("failed to load config from {config_path}"))?;

    let chatto_client = ChattoClient::new(&cfg.chatto_url, &cfg.chatto_token);

    info!(
        rooms = ?cfg.rooms,
        poll_interval = ?cfg.poll_interval,
        "configuration loaded"
    );
    info!(
        base_url = chatto_client.base_url(),
        "chatto client initialized"
    );
    info!("chatto-bot-tidal rust runtime bootstrap complete");

    tokio::signal::ctrl_c()
        .await
        .context("failed to listen for ctrl-c signal")?;
    info!("shutdown signal received");

    Ok(())
}

fn init_tracing() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .with_target(false)
        .compact()
        .init();
}

use std::fs;
use std::future::Future;
use std::io::IsTerminal;
use std::path::PathBuf;
use std::process::{Command as ProcessCommand, Stdio};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, anyhow, bail};
use btleplug::api::{
    Central, Characteristic, Manager as _, Peripheral as _, ScanFilter, WriteType,
};
use btleplug::platform::{Adapter, Manager, Peripheral};
use clap::{Parser, Subcommand};
use dialoguer::{Select, theme::ColorfulTheme};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio::time::{sleep, timeout};
use uuid::Uuid;

const CONTROL_SERVICE_UUID: Uuid = Uuid::from_u128(0x99fa0001338a10248a49009c0215f78a);
const CONTROL_CHARACTERISTIC_UUID: Uuid = Uuid::from_u128(0x99fa0002338a10248a49009c0215f78a);
const POSITION_SERVICE_UUID: Uuid = Uuid::from_u128(0x99fa0020338a10248a49009c0215f78a);
const POSITION_CHARACTERISTIC_UUID: Uuid = Uuid::from_u128(0x99fa0021338a10248a49009c0215f78a);

const CMD_UP: [u8; 2] = [0x47, 0x00];
const CMD_DOWN: [u8; 2] = [0x46, 0x00];
const CMD_STOP: [u8; 2] = [0xFF, 0x00];

const DAEMON_SOCKET_PATH: &str = "/tmp/idasen-desk.sock";
const DAEMON_IDLE_TIMEOUT: Duration = Duration::from_secs(30 * 60);

#[derive(Parser, Debug)]
#[command(name = "desk", about = "IKEA IDÅSEN controller (Rust + btleplug)")]
struct Cli {
    #[command(subcommand)]
    command: Command,

    /// BLE scan time in seconds.
    #[arg(long, global = true, default_value_t = 8)]
    timeout: u64,

    /// Timeout in seconds for connect/discover/read/write operations.
    #[arg(long, global = true, default_value_t = 8)]
    connect_timeout: u64,

    /// Verbose logs.
    #[arg(short, long, global = true)]
    verbose: bool,
}

#[derive(Subcommand, Debug)]
enum Command {
    /// Non-interactive list of nearby desk-like BLE devices.
    List {
        /// JSON output
        #[arg(long)]
        json: bool,

        /// Include all peripherals, not just desk-like ones.
        #[arg(long)]
        all: bool,
    },

    /// Connect and remember desk in ~/.idasen.json.
    ///
    /// If IDENTIFIER is omitted, opens interactive selector.
    Connect { identifier: Option<String> },

    /// Move to configured minimum height.
    Down,

    /// Move to configured maximum height.
    Up,

    /// Move to explicit height.
    MoveTo { height: f32 },

    /// Read current desk height.
    Status,

    #[command(hide = true)]
    Daemon,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
struct Config {
    desk_identifier: Option<String>,
    position_min: f32,
    position_max: f32,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            desk_identifier: None,
            position_min: 0.0,
            position_max: 58.0,
        }
    }
}

#[derive(Debug, Clone)]
struct DiscoveredDesk {
    peripheral: Peripheral,
    id: String,
    address: String,
    name: String,
    rssi: Option<i16>,
    looks_like_desk: bool,
}

#[derive(Debug, Clone, Serialize)]
struct DeskInfo {
    id: String,
    address: String,
    name: String,
    rssi: Option<i16>,
    looks_like_desk: bool,
}

#[derive(Debug, Clone)]
struct DeskConnection {
    peripheral: Peripheral,
    control_char: Characteristic,
    position_char: Characteristic,
    io_timeout: Duration,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum DaemonRequest {
    Ping,
    MoveTo { height: f32 },
    Status,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum DaemonResponse {
    Pong,
    Ok { height: Option<f32> },
    Error { error: String },
}

struct DaemonState {
    adapter: Adapter,
    scan_timeout: Duration,
    op_timeout: Duration,
    verbose: bool,
    cached_identifier: Option<String>,
    cached_connection: Option<DeskConnection>,
}

#[tokio::main(flavor = "multi_thread")]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    let scan_timeout = Duration::from_secs(cli.timeout.max(1));
    let op_timeout = Duration::from_secs(cli.connect_timeout.max(1));

    if matches!(&cli.command, Command::Daemon) {
        run_daemon(scan_timeout, op_timeout, cli.verbose).await?;
        return Ok(());
    }

    let mut config = load_config()?;

    match cli.command {
        Command::List { json, all } => {
            let adapter = get_adapter().await?;
            let discovered = scan_devices(&adapter, scan_timeout, cli.verbose).await?;
            let infos: Vec<DeskInfo> = discovered
                .into_iter()
                .filter(|d| all || d.looks_like_desk)
                .map(|d| d.info())
                .collect();

            if json {
                println!("{}", serde_json::to_string_pretty(&infos)?);
            } else if infos.is_empty() {
                println!("No desks found. Put desk in pairing mode and retry.");
            } else {
                for info in infos {
                    println!(
                        "{} [address: {}, id: {}, RSSI: {}]",
                        if info.name.is_empty() {
                            "Unknown"
                        } else {
                            &info.name
                        },
                        info.address,
                        info.id,
                        info.rssi
                            .map(|x| x.to_string())
                            .unwrap_or_else(|| "?".to_string())
                    );
                }
            }
        }
        Command::Connect { identifier } => {
            let adapter = get_adapter().await?;
            config = connect_and_remember(
                &adapter,
                config,
                identifier,
                scan_timeout,
                op_timeout,
                cli.verbose,
            )
            .await?;
            save_config(&config)?;
        }
        Command::Up => {
            if config.desk_identifier.is_none() {
                let adapter = get_adapter().await?;
                ensure_configured(&adapter, &mut config, scan_timeout, op_timeout, cli.verbose)
                    .await?;
                save_config(&config)?;
            }

            let final_height =
                daemon_move_to(config.position_max, scan_timeout, op_timeout, cli.verbose).await?;
            println!("Desk moved up to {:.2}", final_height);
        }
        Command::Down => {
            if config.desk_identifier.is_none() {
                let adapter = get_adapter().await?;
                ensure_configured(&adapter, &mut config, scan_timeout, op_timeout, cli.verbose)
                    .await?;
                save_config(&config)?;
            }

            let final_height =
                daemon_move_to(config.position_min, scan_timeout, op_timeout, cli.verbose).await?;
            println!("Desk moved down to {:.2}", final_height);
        }
        Command::MoveTo { height } => {
            if config.desk_identifier.is_none() {
                let adapter = get_adapter().await?;
                ensure_configured(&adapter, &mut config, scan_timeout, op_timeout, cli.verbose)
                    .await?;
                save_config(&config)?;
            }

            let target = height.clamp(config.position_min, config.position_max);
            let final_height =
                daemon_move_to(target, scan_timeout, op_timeout, cli.verbose).await?;
            println!("Desk moved to {:.2}", final_height);
        }
        Command::Status => {
            if config.desk_identifier.is_none() {
                let adapter = get_adapter().await?;
                ensure_configured(&adapter, &mut config, scan_timeout, op_timeout, cli.verbose)
                    .await?;
                save_config(&config)?;
            }

            let height = daemon_status(scan_timeout, op_timeout, cli.verbose).await?;
            println!("{height:.2}");
        }
        Command::Daemon => unreachable!("handled before adapter setup"),
    }

    Ok(())
}

impl DiscoveredDesk {
    fn info(&self) -> DeskInfo {
        DeskInfo {
            id: self.id.clone(),
            address: self.address.clone(),
            name: self.name.clone(),
            rssi: self.rssi,
            looks_like_desk: self.looks_like_desk,
        }
    }

    fn best_identifier(&self) -> String {
        if !self.address.is_empty() && self.address != "00:00:00:00:00:00" {
            return self.address.clone();
        }
        if !self.id.is_empty() {
            return self.id.clone();
        }
        self.name.clone()
    }

    fn matches_identifier(&self, value: &str) -> bool {
        let needle = value.trim();
        [self.address.as_str(), self.id.as_str(), self.name.as_str()]
            .into_iter()
            .filter(|s| !s.is_empty())
            .any(|s| s.eq_ignore_ascii_case(needle))
    }
}

fn config_path() -> Result<PathBuf> {
    let home = dirs::home_dir().context("Could not determine home directory")?;
    Ok(home.join(".idasen.json"))
}

fn load_config() -> Result<Config> {
    let path = config_path()?;
    let data = match fs::read_to_string(path) {
        Ok(s) => s,
        Err(_) => return Ok(Config::default()),
    };

    Ok(serde_json::from_str(&data).unwrap_or_default())
}

fn save_config(config: &Config) -> Result<()> {
    let path = config_path()?;
    fs::write(path, format!("{}\n", serde_json::to_string_pretty(config)?))?;
    Ok(())
}

fn daemon_socket_path() -> PathBuf {
    PathBuf::from(DAEMON_SOCKET_PATH)
}

async fn daemon_move_to(
    height: f32,
    scan_timeout: Duration,
    op_timeout: Duration,
    verbose: bool,
) -> Result<f32> {
    let response = daemon_request_with_autostart(
        DaemonRequest::MoveTo { height },
        scan_timeout,
        op_timeout,
        verbose,
    )
    .await?;

    match response {
        DaemonResponse::Ok { height: Some(h) } => Ok(h),
        DaemonResponse::Ok { height: None } => bail!("daemon did not return desk height"),
        DaemonResponse::Error { error } => bail!("{error}"),
        DaemonResponse::Pong => bail!("unexpected daemon response"),
    }
}

async fn daemon_status(scan_timeout: Duration, op_timeout: Duration, verbose: bool) -> Result<f32> {
    let response =
        daemon_request_with_autostart(DaemonRequest::Status, scan_timeout, op_timeout, verbose)
            .await?;

    match response {
        DaemonResponse::Ok { height: Some(h) } => Ok(h),
        DaemonResponse::Ok { height: None } => bail!("daemon did not return desk height"),
        DaemonResponse::Error { error } => bail!("{error}"),
        DaemonResponse::Pong => bail!("unexpected daemon response"),
    }
}

async fn daemon_request_with_autostart(
    request: DaemonRequest,
    scan_timeout: Duration,
    op_timeout: Duration,
    verbose: bool,
) -> Result<DaemonResponse> {
    if let Ok(response) = send_daemon_request(&request).await {
        return Ok(response);
    }

    if verbose {
        eprintln!("Daemon not running, starting background daemon...");
    }

    spawn_daemon(scan_timeout, op_timeout, verbose)?;

    for _ in 0..40 {
        sleep(Duration::from_millis(150)).await;
        if let Ok(DaemonResponse::Pong) = send_daemon_request(&DaemonRequest::Ping).await {
            return send_daemon_request(&request).await;
        }
    }

    bail!("failed to start daemon")
}

fn spawn_daemon(scan_timeout: Duration, op_timeout: Duration, verbose: bool) -> Result<()> {
    let exe = std::env::current_exe().context("could not resolve current executable")?;

    let mut cmd = ProcessCommand::new(exe);
    cmd.arg("--timeout")
        .arg(scan_timeout.as_secs().to_string())
        .arg("--connect-timeout")
        .arg(op_timeout.as_secs().to_string());

    if verbose {
        cmd.arg("--verbose");
    }

    cmd.arg("daemon")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());

    cmd.spawn().context("failed to spawn daemon")?;
    Ok(())
}

async fn send_daemon_request(request: &DaemonRequest) -> Result<DaemonResponse> {
    let path = daemon_socket_path();

    let mut stream = with_timeout(
        Duration::from_secs(2),
        "connect daemon socket",
        UnixStream::connect(&path),
    )
    .await?;

    let payload = serde_json::to_string(request)?;
    with_timeout(
        Duration::from_secs(2),
        "write daemon request",
        stream.write_all(payload.as_bytes()),
    )
    .await?;
    with_timeout(
        Duration::from_secs(2),
        "write daemon newline",
        stream.write_all(b"\n"),
    )
    .await?;

    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    let bytes = with_timeout(
        Duration::from_secs(20),
        "read daemon response",
        reader.read_line(&mut line),
    )
    .await?;

    if bytes == 0 {
        bail!("daemon closed socket without response")
    }

    let response: DaemonResponse =
        serde_json::from_str(line.trim_end()).context("failed to parse daemon response")?;
    Ok(response)
}

async fn run_daemon(scan_timeout: Duration, op_timeout: Duration, verbose: bool) -> Result<()> {
    let adapter = get_adapter().await?;
    let socket_path = daemon_socket_path();

    if socket_path.exists() {
        let _ = fs::remove_file(&socket_path);
    }

    let listener = UnixListener::bind(&socket_path)
        .with_context(|| format!("failed to bind daemon socket at {}", socket_path.display()))?;

    if verbose {
        eprintln!(
            "Daemon started at {} (idle timeout: {}m)",
            socket_path.display(),
            DAEMON_IDLE_TIMEOUT.as_secs() / 60
        );
    }

    let mut state = DaemonState {
        adapter,
        scan_timeout,
        op_timeout,
        verbose,
        cached_identifier: None,
        cached_connection: None,
    };

    let mut last_activity = Instant::now();

    loop {
        if last_activity.elapsed() >= DAEMON_IDLE_TIMEOUT {
            if verbose {
                eprintln!("Daemon idle timeout reached, shutting down.");
            }
            break;
        }

        match timeout(Duration::from_secs(2), listener.accept()).await {
            Ok(Ok((stream, _))) => {
                last_activity = Instant::now();
                if let Err(err) = handle_daemon_client(stream, &mut state).await {
                    if verbose {
                        eprintln!("daemon request failed: {err:#}");
                    }
                }
            }
            Ok(Err(err)) => {
                if verbose {
                    eprintln!("daemon accept error: {err}");
                }
            }
            Err(_) => {}
        }
    }

    if let Some(conn) = state.cached_connection.take() {
        let _ = timeout(Duration::from_secs(2), conn.peripheral.disconnect()).await;
    }

    let _ = fs::remove_file(&socket_path);
    Ok(())
}

async fn handle_daemon_client(stream: UnixStream, state: &mut DaemonState) -> Result<()> {
    let mut reader = BufReader::new(stream);
    let mut line = String::new();

    let read = with_timeout(
        Duration::from_secs(5),
        "read daemon client request",
        reader.read_line(&mut line),
    )
    .await?;

    if read == 0 {
        return Ok(());
    }

    let request: DaemonRequest =
        serde_json::from_str(line.trim_end()).context("invalid daemon request")?;

    let response = state.handle_request(request).await;
    let payload = serde_json::to_string(&response)?;

    let mut stream = reader.into_inner();
    with_timeout(
        Duration::from_secs(5),
        "write daemon response",
        stream.write_all(payload.as_bytes()),
    )
    .await?;
    with_timeout(
        Duration::from_secs(5),
        "write daemon response newline",
        stream.write_all(b"\n"),
    )
    .await?;

    Ok(())
}

impl DaemonState {
    async fn handle_request(&mut self, request: DaemonRequest) -> DaemonResponse {
        match request {
            DaemonRequest::Ping => DaemonResponse::Pong,
            DaemonRequest::Status => match self.get_connection().await {
                Ok(conn) => match read_height(&conn).await {
                    Ok(height) => DaemonResponse::Ok {
                        height: Some(height),
                    },
                    Err(err) => {
                        self.cached_connection = None;
                        DaemonResponse::Error {
                            error: format!("{err:#}"),
                        }
                    }
                },
                Err(err) => DaemonResponse::Error {
                    error: format!("{err:#}"),
                },
            },
            DaemonRequest::MoveTo { height } => match self.get_connection().await {
                Ok(conn) => {
                    let cfg = load_config().unwrap_or_default();
                    let target = height.clamp(cfg.position_min, cfg.position_max);
                    match move_to_height(&conn, target, self.verbose).await {
                        Ok(final_height) => DaemonResponse::Ok {
                            height: Some(final_height),
                        },
                        Err(err) => {
                            self.cached_connection = None;
                            DaemonResponse::Error {
                                error: format!("{err:#}"),
                            }
                        }
                    }
                }
                Err(err) => DaemonResponse::Error {
                    error: format!("{err:#}"),
                },
            },
        }
    }

    async fn get_connection(&mut self) -> Result<DeskConnection> {
        let cfg = load_config()?;
        let identifier = cfg
            .desk_identifier
            .clone()
            .context("No desk configured. Run `desk connect` first.")?;

        if self.cached_identifier.as_deref() == Some(identifier.as_str()) {
            if let Some(conn) = &self.cached_connection {
                let connected = with_timeout(
                    Duration::from_secs(2),
                    "check connection",
                    conn.peripheral.is_connected(),
                )
                .await
                .unwrap_or(false);
                if connected {
                    return Ok(conn.clone());
                }
            }
        }

        if let Some(old) = self.cached_connection.take() {
            let _ = timeout(Duration::from_secs(2), old.peripheral.disconnect()).await;
        }

        let desk =
            resolve_configured_desk(&self.adapter, &cfg, self.scan_timeout, self.verbose).await?;
        let conn = connect_to_desk(&desk.peripheral, self.op_timeout, self.verbose).await?;

        self.cached_identifier = Some(identifier);
        self.cached_connection = Some(conn.clone());
        Ok(conn)
    }
}

async fn get_adapter() -> Result<Adapter> {
    let manager = Manager::new().await?;
    let adapters = manager.adapters().await?;
    adapters.into_iter().next().context("No BLE adapter found")
}

fn looks_like_desk(name: &str, services: &[Uuid]) -> bool {
    let lower = name.to_ascii_lowercase();
    lower.contains("desk")
        || lower.contains("idasen")
        || services.contains(&CONTROL_SERVICE_UUID)
        || services.contains(&POSITION_SERVICE_UUID)
}

async fn scan_devices(
    adapter: &Adapter,
    scan_timeout: Duration,
    verbose: bool,
) -> Result<Vec<DiscoveredDesk>> {
    if verbose {
        eprintln!("Scanning BLE for {}s...", scan_timeout.as_secs());
    }

    with_timeout(
        Duration::from_secs(4),
        "start scan",
        adapter.start_scan(ScanFilter::default()),
    )
    .await?;

    sleep(scan_timeout).await;

    let peripherals = with_timeout(
        Duration::from_secs(4),
        "read peripherals",
        adapter.peripherals(),
    )
    .await?;

    let mut out = Vec::new();
    for peripheral in peripherals {
        let props = peripheral
            .properties()
            .await
            .context("failed to read peripheral properties")?;

        let Some(props) = props else {
            continue;
        };

        let name = props.local_name.unwrap_or_default();
        let services = props.services;

        out.push(DiscoveredDesk {
            id: peripheral.id().to_string(),
            address: props.address.to_string(),
            rssi: props.rssi,
            looks_like_desk: looks_like_desk(&name, &services),
            name,
            peripheral,
        });
    }

    out.sort_by(|a, b| b.rssi.unwrap_or(i16::MIN).cmp(&a.rssi.unwrap_or(i16::MIN)));

    let _ = with_timeout(Duration::from_secs(3), "stop scan", adapter.stop_scan()).await;
    sleep(Duration::from_millis(450)).await;

    Ok(out)
}

async fn connect_and_remember(
    adapter: &Adapter,
    mut config: Config,
    identifier: Option<String>,
    scan_timeout: Duration,
    op_timeout: Duration,
    verbose: bool,
) -> Result<Config> {
    eprintln!("Scanning for desks...");
    let discovered = scan_devices(adapter, scan_timeout, verbose).await?;

    let chosen = if let Some(identifier) = identifier {
        discovered
            .into_iter()
            .find(|d| d.matches_identifier(&identifier))
            .with_context(|| format!("No desk/peripheral found for identifier: {identifier}"))?
    } else {
        let desks: Vec<DiscoveredDesk> = discovered
            .into_iter()
            .filter(|d| d.looks_like_desk)
            .collect();
        choose_desk_interactively(desks)?
    };

    eprintln!("Connecting to {}...", chosen.best_identifier());
    let conn = connect_to_desk(&chosen.peripheral, op_timeout, verbose)
        .await
        .with_context(|| format!("Failed to connect to {}", chosen.best_identifier()))?;

    let height = read_height(&conn).await?;
    let remembered = chosen.best_identifier();
    config.desk_identifier = Some(remembered.clone());

    println!("Connected to desk \"{}\" at {:.2}", remembered, height);
    println!("Saved in {}", config_path()?.display());

    let _ = conn.peripheral.disconnect().await;

    Ok(config)
}

fn choose_desk_interactively(desks: Vec<DiscoveredDesk>) -> Result<DiscoveredDesk> {
    if desks.is_empty() {
        bail!("No desk found. Put desk in pairing mode and retry.");
    }

    if !std::io::stdin().is_terminal() {
        bail!("No TTY available. Run: desk connect <identifier>");
    }

    let items: Vec<String> = desks
        .iter()
        .map(|d| {
            format!(
                "{} [address: {}, id: {}, RSSI: {}]",
                if d.name.is_empty() {
                    "Unknown".to_string()
                } else {
                    d.name.clone()
                },
                d.address,
                d.id,
                d.rssi
                    .map(|x| x.to_string())
                    .unwrap_or_else(|| "?".to_string())
            )
        })
        .collect();

    let selected = Select::with_theme(&ColorfulTheme::default())
        .with_prompt("Select desk")
        .items(&items)
        .default(0)
        .interact()?;

    Ok(desks[selected].clone())
}

async fn ensure_configured(
    adapter: &Adapter,
    config: &mut Config,
    scan_timeout: Duration,
    op_timeout: Duration,
    verbose: bool,
) -> Result<()> {
    if config.desk_identifier.is_some() {
        return Ok(());
    }

    if verbose {
        eprintln!("No desk configured. Running connect flow...");
    }

    *config = connect_and_remember(
        adapter,
        config.clone(),
        None,
        scan_timeout,
        op_timeout,
        verbose,
    )
    .await?;

    Ok(())
}

async fn resolve_configured_desk(
    adapter: &Adapter,
    config: &Config,
    scan_timeout: Duration,
    verbose: bool,
) -> Result<DiscoveredDesk> {
    let identifier = config
        .desk_identifier
        .as_deref()
        .context("No desk configured. Run `desk connect` first.")?;

    if verbose {
        eprintln!("Scanning for configured desk: {identifier}");
    }

    let discovered = scan_devices(adapter, scan_timeout, verbose).await?;
    let mut matches: Vec<DiscoveredDesk> = discovered
        .into_iter()
        .filter(|d| d.matches_identifier(identifier))
        .collect();

    if matches.is_empty() {
        bail!(
            "Configured desk '{}' not found. Put desk in pairing mode and run `desk connect`.",
            identifier
        );
    }

    matches.sort_by(|a, b| b.rssi.unwrap_or(i16::MIN).cmp(&a.rssi.unwrap_or(i16::MIN)));

    let chosen = matches.remove(0);
    if verbose {
        eprintln!(
            "Using peripheral: {} [address: {}, id: {}, RSSI: {}]",
            if chosen.name.is_empty() {
                "Unknown"
            } else {
                &chosen.name
            },
            chosen.address,
            chosen.id,
            chosen
                .rssi
                .map(|x| x.to_string())
                .unwrap_or_else(|| "?".to_string())
        );
    }

    Ok(chosen)
}

async fn connect_to_desk(
    peripheral: &Peripheral,
    op_timeout: Duration,
    verbose: bool,
) -> Result<DeskConnection> {
    let mut last_err: Option<anyhow::Error> = None;

    for attempt in 1..=3 {
        if verbose {
            eprintln!("Connect attempt {attempt}/3");
            eprintln!("Allowing BLE stack to settle...");
        }
        sleep(Duration::from_millis(350)).await;

        if verbose {
            eprintln!("Checking existing connection state...");
        }

        let connected = with_timeout(op_timeout, "check connection", peripheral.is_connected())
            .await
            .unwrap_or(false);

        if connected {
            if verbose {
                eprintln!("Peripheral was already connected, clearing stale connection...");
            }
            match timeout(Duration::from_secs(2), peripheral.disconnect()).await {
                Ok(_) => {
                    sleep(Duration::from_millis(200)).await;
                }
                Err(_) => {
                    if verbose {
                        eprintln!("Disconnect timed out, proceeding with fresh connect attempt...");
                    }
                }
            }
        }

        if verbose {
            eprintln!("Connecting BLE peripheral...");
        }
        if let Err(err) = with_timeout(op_timeout, "connect", peripheral.connect()).await {
            last_err = Some(err);
            if attempt < 3 {
                sleep(Duration::from_millis(600)).await;
                continue;
            }
            break;
        }

        if verbose {
            eprintln!("Discovering services...");
        }
        if let Err(err) = with_timeout(
            op_timeout,
            "discover services",
            peripheral.discover_services(),
        )
        .await
        {
            last_err = Some(err);
            if attempt < 3 {
                let _ = timeout(Duration::from_secs(2), peripheral.disconnect()).await;
                sleep(Duration::from_millis(600)).await;
                continue;
            }
            break;
        }

        let chars = peripheral.characteristics();

        let Some(control_char) = chars
            .iter()
            .find(|c| c.uuid == CONTROL_CHARACTERISTIC_UUID)
            .cloned()
        else {
            last_err = Some(anyhow!("Desk control characteristic not found"));
            if attempt < 3 {
                let _ = timeout(Duration::from_secs(2), peripheral.disconnect()).await;
                sleep(Duration::from_millis(600)).await;
                continue;
            }
            break;
        };

        let Some(position_char) = chars
            .iter()
            .find(|c| c.uuid == POSITION_CHARACTERISTIC_UUID)
            .cloned()
        else {
            last_err = Some(anyhow!("Desk position characteristic not found"));
            if attempt < 3 {
                let _ = timeout(Duration::from_secs(2), peripheral.disconnect()).await;
                sleep(Duration::from_millis(600)).await;
                continue;
            }
            break;
        };

        return Ok(DeskConnection {
            peripheral: peripheral.clone(),
            control_char,
            position_char,
            io_timeout: op_timeout,
        });
    }

    Err(last_err.unwrap_or_else(|| anyhow!("Unable to connect to peripheral")))
}

async fn read_height(conn: &DeskConnection) -> Result<f32> {
    let data = with_timeout(
        conn.io_timeout,
        "read desk height",
        conn.peripheral.read(&conn.position_char),
    )
    .await?;

    if data.len() < 2 {
        bail!("Desk position response too short");
    }

    let raw = i16::from_le_bytes([data[0], data[1]]);
    Ok(raw as f32 / 100.0)
}

async fn send_control(conn: &DeskConnection, cmd: &[u8]) -> Result<()> {
    with_timeout(
        conn.io_timeout,
        "write desk control",
        conn.peripheral
            .write(&conn.control_char, cmd, WriteType::WithoutResponse),
    )
    .await?;
    Ok(())
}

async fn move_to_height(conn: &DeskConnection, target: f32, verbose: bool) -> Result<f32> {
    let mut current = read_height(conn).await?;

    if verbose {
        eprintln!("Current: {:.2}, target: {:.2}", current, target);
    }

    if (current - target).abs() <= 0.8 {
        return Ok(current);
    }

    let moving_up = target > current;
    let command = if moving_up { &CMD_UP } else { &CMD_DOWN };

    let deadline = Instant::now() + Duration::from_secs(45);
    let mut last_command_sent = Instant::now() - Duration::from_secs(1);
    let mut last_pos = current;
    let mut stagnant = 0u8;

    while Instant::now() < deadline {
        if moving_up {
            if current >= target - 0.8 {
                break;
            }
        } else if current <= target + 0.8 {
            break;
        }

        if last_command_sent.elapsed() >= Duration::from_millis(300) {
            send_control(conn, command).await?;
            last_command_sent = Instant::now();
        }

        sleep(Duration::from_millis(120)).await;

        match read_height(conn).await {
            Ok(h) => {
                current = h;
                if verbose {
                    eprintln!("height: {:.2}", current);
                }

                if (current - last_pos).abs() < 0.02 {
                    stagnant = stagnant.saturating_add(1);
                } else {
                    stagnant = 0;
                }

                last_pos = current;

                if stagnant >= 12 {
                    if verbose {
                        eprintln!("Movement stalled; stopping.");
                    }
                    break;
                }
            }
            Err(err) => {
                if verbose {
                    eprintln!("Read failed during move: {err:#}");
                }
            }
        }
    }

    let _ = send_control(conn, &CMD_STOP).await;
    sleep(Duration::from_millis(150)).await;

    Ok(read_height(conn).await.unwrap_or(current))
}

async fn with_timeout<T, F, E>(dur: Duration, label: &str, fut: F) -> Result<T>
where
    F: Future<Output = std::result::Result<T, E>>,
    E: std::error::Error + Send + Sync + 'static,
{
    match timeout(dur, fut).await {
        Ok(Ok(value)) => Ok(value),
        Ok(Err(err)) => Err(anyhow!(err)).context(label.to_string()),
        Err(_) => bail!("{} timed out after {:.1}s", label, dur.as_secs_f32()),
    }
}

//! Opt-in, synthetic-only Go↔Rust session recovery proof. The fixture store
//! implements the real SecureStore durability contract in one private test
//! directory; production never gains a plaintext persistence fallback.
#![deny(clippy::wildcard_enum_match_arm)]

use home_client_session::{Phase, SecureStore, ServerRevocation, SessionCoordinator, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::auth::v1 as auth;
use serde::{Deserialize, Serialize, de::IntoDeserializer};
use std::{
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;

#[derive(Clone, Copy, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Scenario {
    Lose,
    Restore,
    RestoreOutage,
    RestoreBareRenewal,
    BarePreflight,
    RestoreRevoked,
    RestoreExpired,
    Cleanup,
}
impl std::str::FromStr for Scenario {
    type Err = serde::de::value::Error;
    fn from_str(value: &str) -> std::result::Result<Self, Self::Err> {
        Self::deserialize(value.into_deserializer())
    }
}
#[derive(Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "kebab-case")]
enum Action {
    DropRefresh,
    Outage,
    Restore,
    BarePreflight,
    BareRenewal,
    Revoke,
    Expire,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: String,
    origin: String,
    ca_pem: String,
    login_name: String,
    password: String,
    account_id: String,
    namespace_nonce: String,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Command {
    sequence: u64,
    action: Action,
}
struct Control(u64);
impl Control {
    async fn command(&mut self, action: Action) -> Result<()> {
        self.0 = self.0.checked_add(1).ok_or("control sequence exhausted")?;
        println!(
            "NHF032_CONTROL {}",
            serde_json::to_string(&Command {
                sequence: self.0,
                action
            })?
        );
        std::io::stdout().flush()?;
        let line = tokio::task::spawn_blocking(|| {
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).map(|_| line)
        })
        .await??;
        let response: Command = serde_json::from_str(&line)?;
        require(
            response.sequence == self.0 && response.action == action,
            "wrong control acknowledgement",
        )
    }
}

// Test-only, one explicitly synthetic directory and one sequential probe.
// save fsyncs both payload and rename before acknowledging durable ownership.
struct FixtureStore {
    directory: PathBuf,
}
impl FixtureStore {
    fn path(&self) -> PathBuf {
        self.directory.join("synthetic-session.bin")
    }
    fn checked_path(&self) -> std::result::Result<PathBuf, StoreFailure> {
        let path = self.path();
        match fs::symlink_metadata(&path) {
            Ok(meta) if !meta.is_file() || meta.file_type().is_symlink() => {
                Err(StoreFailure::Corrupt)
            }
            Ok(_) => Ok(path),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(path),
            Err(_) => Err(StoreFailure::Unavailable),
        }
    }
}
impl SecureStore for FixtureStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        let path = self.checked_path()?;
        let file = match File::open(path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(_) => return Err(StoreFailure::Unavailable),
        };
        let mut bytes = Zeroizing::new(Vec::new());
        file.take(32769)
            .read_to_end(&mut bytes)
            .map_err(|_| StoreFailure::Unavailable)?;
        if bytes.len() > 32768 {
            return Err(StoreFailure::Corrupt);
        }
        Ok(Some(bytes))
    }
    fn save(&self, bytes: &[u8]) -> std::result::Result<(), StoreFailure> {
        use std::os::unix::fs::OpenOptionsExt;
        let path = self.checked_path()?;
        let writing = self.directory.join("synthetic-session.writing");
        let result = (|| {
            let mut file = OpenOptions::new()
                .create_new(true)
                .write(true)
                .mode(0o600)
                .open(&writing)?;
            file.write_all(bytes)?;
            file.sync_all()?;
            drop(file);
            fs::rename(&writing, path)?;
            File::open(&self.directory)?.sync_all()
        })();
        result.map_err(|_: std::io::Error| StoreFailure::Unavailable)
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        let path = self.checked_path()?;
        match fs::remove_file(path) {
            Ok(()) => File::open(&self.directory)
                .and_then(|dir| dir.sync_all())
                .map_err(|_| StoreFailure::Unavailable),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(_) => Err(StoreFailure::Unavailable),
        }
    }
}
fn require(value: bool, message: &str) -> Result<()> {
    if value { Ok(()) } else { Err(message.into()) }
}
async fn protected_read(
    connection: &Connection,
    owner: &SessionCoordinator,
    fixture: &Fixture,
) -> Result<()> {
    let lease = owner.access(&CancellationToken::new()).await?;
    let response = connection
        .auth()
        .list_sessions(
            auth::ListSessionsRequest {
                account_id: auth::AccountId {
                    value: fixture.account_id.clone(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(5),
            &lease.cancellation,
        )
        .await?;
    require(
        response
            .sessions
            .iter()
            .any(|session| session.id.value == lease.session_id().as_str()),
        "session missing from real protected read",
    )
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    require(
        args.len() == 4,
        "synthetic fixture, typed scenario and sequence required",
    )?;
    let scenario: Scenario = args[2].parse()?;
    let mut control = Control(args[3].parse()?);
    let ready = Path::new(&args[1]);
    let fixture: Fixture = serde_json::from_slice(&fs::read(ready)?)?;
    require(
        fixture.mode == "synthetic-only"
            && fixture.origin.starts_with("https://127.0.0.1:")
            && !fixture.namespace_nonce.is_empty(),
        "isolated synthetic loopback fixture required",
    )?;
    let store = Arc::new(FixtureStore {
        directory: ready
            .parent()
            .ok_or("fixture parent missing")?
            .canonicalize()?,
    });
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let owner = SessionCoordinator::new(connection.auth(), store.clone());
    let outcome=async {
  match scenario {
   Scenario::Lose|Scenario::BarePreflight=>{
    require(store.load()?.is_none(),"preserve existing synthetic record")?;
    owner.login(fixture.login_name.clone(),Zeroizing::new(fixture.password.clone())).await?;
    protected_read(&connection,&owner,&fixture).await?;
    // The existing NHF003 proxy advertises33s and the client uncertainty
    // consumes30s, leaving3s; the real server access still lasts15m.
    tokio::time::sleep(Duration::from_millis(4100)).await;
   }
   Scenario::Restore|Scenario::RestoreOutage|Scenario::RestoreBareRenewal|Scenario::RestoreRevoked|Scenario::RestoreExpired=>{
    require(store.load()?.is_some(),"pending synthetic record missing before restart")?;
    owner.restore().await?;
   }
   Scenario::Cleanup=>{
    owner.logout_local().await?;
    require(store.load()?.is_none(),"synthetic record cleanup failed")?;
    return Ok(serde_json::json!({"scenario":scenario,"local_deleted":true}));
   }
  }
  match scenario {
   Scenario::Lose=>{
    control.command(Action::DropRefresh).await?;
    require(owner.access(&CancellationToken::new()).await.is_err(),"lost response unexpectedly yielded access")?;
    require(owner.subscribe().borrow().phase==Phase::Offline&&store.load()?.is_some(),"committed response loss discarded durable session")?;
    Ok(serde_json::json!({"scenario":scenario,"pending_record_saved":true,"offline":true,"local_deleted":false}))
   }
   Scenario::RestoreRevoked|Scenario::RestoreExpired=>{
    control.command(match scenario {Scenario::RestoreRevoked=>Action::Revoke,Scenario::RestoreExpired=>Action::Expire,Scenario::Lose|Scenario::Restore|Scenario::RestoreOutage|Scenario::RestoreBareRenewal|Scenario::BarePreflight|Scenario::Cleanup=>return Err("unhandled terminal scenario".into())}).await?;
    require(owner.access(&CancellationToken::new()).await.is_err(),"terminal credential accepted")?;
    require(owner.subscribe().borrow().phase==Phase::NeedsLogin&&store.load()?.is_none(),"typed definitive terminal error did not clear session")?;
    Ok(serde_json::json!({"scenario":scenario,"typed_terminal_stopped":true,"local_deleted":true}))
   }
   Scenario::Restore|Scenario::RestoreOutage|Scenario::RestoreBareRenewal|Scenario::BarePreflight=>{
    match scenario {
     Scenario::RestoreOutage|Scenario::RestoreBareRenewal=>{
      let action=if matches!(scenario,Scenario::RestoreOutage){Action::Outage}else{Action::BareRenewal};
      control.command(action).await?;
      require(owner.access(&CancellationToken::new()).await.is_err(),"synthetic outage unexpectedly yielded access")?;
      require(store.load()?.is_some(),"transient or untyped error destroyed saved session")?;
      control.command(Action::Restore).await?;
     }
     Scenario::BarePreflight=>{control.command(Action::BarePreflight).await?;owner.access(&CancellationToken::new()).await?;control.command(Action::Restore).await?;}
     Scenario::Restore=>{}
     Scenario::Lose|Scenario::RestoreRevoked|Scenario::RestoreExpired|Scenario::Cleanup=>return Err("unhandled recoverable scenario".into()),
    }
    protected_read(&connection,&owner,&fixture).await?;
    require(owner.subscribe().borrow().phase==Phase::Ready,"restored session was not ready")?;
    require(owner.logout().await?==ServerRevocation::Acknowledged&&store.load()?.is_none(),"real revocation or local cleanup failed")?;
    Ok(serde_json::json!({"scenario":scenario,"protected_read":true,"restart_recovered":!matches!(scenario,Scenario::BarePreflight),"local_deleted":true,"server_revoked":true}))
   }
   Scenario::Cleanup=>Err("cleanup already returned".into()),
  }
 }.await;
    owner.close().await;
    let result: Result<serde_json::Value> = outcome;
    println!("NHF032_RESULT {}", result?);
    Ok(())
}

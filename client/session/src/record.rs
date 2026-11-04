use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::api::v2 as auth};
use zeroize::{Zeroize, Zeroizing};

const MAGIC: &[u8; 8] = b"HWSess03";
const HEADER: usize = 21;
const LIMIT: usize = 32 * 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RecordFailure {
    Invalid,
    RefreshConsumed,
    ClockMovedBackwards,
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum RefreshState {
    Available,
    /// The access bearer was refused without a definitive session error.
    AccessRejected,
    Consumed,
    Pending([u8; 32]),
}

impl std::fmt::Debug for RefreshState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Available => "Available",
            Self::AccessRejected => "AccessRejected",
            Self::Consumed => "Consumed",
            Self::Pending(_) => "Pending([REDACTED])",
        })
    }
}

/// Secret-bearing values deliberately have no Debug implementation. This local
/// envelope wraps the existing Buffa token message, not a second network DTO.
pub struct SecretRecord {
    pub(crate) tokens: auth::SessionTokens,
    pub(crate) admitted_at: Timestamp,
    pub(crate) refresh: RefreshState,
}

impl SecretRecord {
    pub fn new(tokens: auth::SessionTokens, admitted_at: Timestamp) -> Result<Self, RecordFailure> {
        let record = Self {
            tokens,
            admitted_at,
            refresh: RefreshState::Available,
        };
        auth_contract::validate_received_session_tokens(&record.tokens, &record.admitted_at)
            .map_err(|_| RecordFailure::Invalid)?;
        Ok(record)
    }

    /// Explicit local destruction of a refresh credential; never dispatched again.
    pub fn consume_refresh(&mut self) -> Result<(), RecordFailure> {
        if self.refresh != RefreshState::Available {
            return Err(RecordFailure::RefreshConsumed);
        }
        self.refresh = RefreshState::Consumed;
        Ok(())
    }

    pub fn encode(&self) -> Zeroizing<Vec<u8>> {
        let mut bytes = Zeroizing::new(Vec::new());
        bytes.extend_from_slice(MAGIC);
        bytes.push(match self.refresh {
            RefreshState::Available => 0,
            RefreshState::AccessRejected => 3,
            RefreshState::Consumed => 1,
            RefreshState::Pending(_) => 2,
        });
        bytes.extend_from_slice(&self.admitted_at.seconds.to_le_bytes());
        bytes.extend_from_slice(&self.admitted_at.nanos.to_le_bytes());
        if let RefreshState::Pending(key) = self.refresh {
            bytes.extend_from_slice(&key);
        }
        self.tokens.encode(&mut *bytes);
        bytes
    }

    pub fn decode(bytes: &[u8], now: &Timestamp) -> Result<Self, RecordFailure> {
        if !(HEADER..=LIMIT).contains(&bytes.len()) || &bytes[..8] != MAGIC {
            return Err(RecordFailure::Invalid);
        }
        let refresh = match bytes[8] {
            0 => RefreshState::Available,
            1 => RefreshState::Consumed,
            3 if &bytes[..8] == MAGIC => RefreshState::AccessRejected,
            2 if &bytes[..8] == MAGIC && bytes.len() >= HEADER + 32 => {
                let key: [u8; 32] = bytes[HEADER..HEADER + 32]
                    .try_into()
                    .map_err(|_| RecordFailure::Invalid)?;
                if key == [0; 32] {
                    return Err(RecordFailure::Invalid);
                }
                RefreshState::Pending(key)
            }
            _ => return Err(RecordFailure::Invalid),
        };
        let seconds = i64::from_le_bytes(
            bytes[9..17]
                .try_into()
                .map_err(|_| RecordFailure::Invalid)?,
        );
        let nanos = i32::from_le_bytes(
            bytes[17..21]
                .try_into()
                .map_err(|_| RecordFailure::Invalid)?,
        );
        let payload = HEADER
            + if matches!(refresh, RefreshState::Pending(_)) {
                32
            } else {
                0
            };
        let tokens = auth::SessionTokens::decode_from_slice(&bytes[payload..])
            .map_err(|_| RecordFailure::Invalid)?;
        let mut record = Self::new(
            tokens,
            Timestamp {
                seconds,
                nanos,
                ..Default::default()
            },
        )?;
        record.refresh = refresh;
        if (now.seconds, now.nanos) < (record.admitted_at.seconds, record.admitted_at.nanos) {
            return Err(RecordFailure::ClockMovedBackwards);
        }
        // The server owns sliding inactivity. A lost renewal response may have
        // extended its deadline beyond this saved metadata; ask it to recover.
        if refresh == RefreshState::Consumed {
            return Err(RecordFailure::RefreshConsumed);
        }
        Ok(record)
    }

    pub fn access_expired(&self, now: &Timestamp) -> bool {
        if matches!(
            self.refresh,
            RefreshState::Pending(_) | RefreshState::AccessRejected
        ) {
            return true;
        }
        let Ok(latest) = auth_contract::interactive_expiration_now(now) else {
            return true;
        };
        (
            self.tokens.access_expire_time.seconds,
            self.tokens.access_expire_time.nanos,
        ) <= (latest.seconds, latest.nanos)
    }

    pub fn session_expired(&self, now: &Timestamp) -> bool {
        let Ok(latest) = auth_contract::interactive_expiration_now(now) else {
            return true;
        };
        (
            self.tokens.session.expire_time.seconds,
            self.tokens.session.expire_time.nanos,
        ) <= (latest.seconds, latest.nanos)
    }
}

impl Drop for SecretRecord {
    fn drop(&mut self) {
        self.tokens.access_token.zeroize();
        self.tokens.refresh_token.zeroize();
        if let RefreshState::Pending(key) = &mut self.refresh {
            key.zeroize();
        }
    }
}

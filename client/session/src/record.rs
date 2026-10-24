use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::auth::v1 as auth};
use zeroize::{Zeroize, Zeroizing};

const MAGIC: &[u8; 8] = b"HWSess01";
const HEADER: usize = 21;
const LIMIT: usize = 32 * 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RecordFailure {
    Invalid,
    Expired,
    RefreshConsumed,
    ClockMovedBackwards,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RefreshState {
    Available,
    Consumed,
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

    /// Must be securely committed BEFORE dispatching the one-use refresh.
    pub fn consume_refresh(&mut self) -> Result<(), RecordFailure> {
        if self.refresh == RefreshState::Consumed {
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
            RefreshState::Consumed => 1,
        });
        bytes.extend_from_slice(&self.admitted_at.seconds.to_le_bytes());
        bytes.extend_from_slice(&self.admitted_at.nanos.to_le_bytes());
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
        let tokens = auth::SessionTokens::decode_from_slice(&bytes[HEADER..])
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
        if record.session_expired(now) {
            return Err(RecordFailure::Expired);
        }
        if refresh == RefreshState::Consumed {
            return Err(RecordFailure::RefreshConsumed);
        }
        Ok(record)
    }

    pub fn access_expired(&self, now: &Timestamp) -> bool {
        let Ok(latest) = auth_contract::interactive_expiration_now(now) else {
            return true;
        };
        (
            self.tokens.access_expires_at.seconds,
            self.tokens.access_expires_at.nanos,
        ) <= (latest.seconds, latest.nanos)
    }

    pub fn session_expired(&self, now: &Timestamp) -> bool {
        let Ok(latest) = auth_contract::interactive_expiration_now(now) else {
            return true;
        };
        (
            self.tokens.session.expires_at.seconds,
            self.tokens.session.expires_at.nanos,
        ) <= (latest.seconds, latest.nanos)
    }
}

impl Drop for SecretRecord {
    fn drop(&mut self) {
        self.tokens.access_token.zeroize();
        self.tokens.refresh_token.zeroize();
    }
}

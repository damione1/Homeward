use std::time::Duration;

use connectrpc::ErrorCode;
use home_wire_contract::home::state::v1 as state;
use tokio_util::sync::CancellationToken;

use crate::{AccessToken, Failure, FailureKind, StateTransport, StateWatch};

/// Finite read-only reconnect budget. The cache owns the last applied cursor;
/// this policy never advances it or retries a command/session refresh.
#[derive(Clone, Copy)]
pub struct ReconnectPolicy {
    attempts: u8,
    initial: Duration,
    maximum: Duration,
}

impl ReconnectPolicy {
    pub fn new(attempts: u8, initial: Duration, maximum: Duration) -> Result<Self, Failure> {
        if !(1..=8).contains(&attempts)
            || initial.is_zero()
            || initial > maximum
            || maximum > Duration::from_secs(30)
        {
            return Err(Failure::invalid_configuration());
        }
        Ok(Self {
            attempts,
            initial,
            maximum,
        })
    }

    /// Called after a failed attempt (numbered from one). False exhausts the
    /// budget; cancellation interrupts the wait without starting another call.
    pub async fn wait_after(
        &self,
        attempt: u8,
        cancel: &CancellationToken,
    ) -> Result<bool, Failure> {
        if cancel.is_cancelled() {
            return Err(Failure::cancelled());
        }
        if attempt == 0 || attempt >= self.attempts {
            return Ok(false);
        }
        let delay = self
            .initial
            .saturating_mul(1 << (attempt - 1))
            .min(self.maximum);
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            () = tokio::time::sleep(delay) => Ok(true),
        }
    }
}

impl StateTransport {
    /// Reopen only this supplied read cursor. Transport errors/deadlines can be
    /// retried; TLS identity, authentication and authorization failures cannot.
    /// A successful resync/EOF remains visible to the caller as such.
    pub async fn reconnect_watch(
        &self,
        request: state::WatchViewRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
        policy: ReconnectPolicy,
    ) -> Result<StateWatch, Failure> {
        let mut attempt = 1;
        loop {
            match self
                .watch_view(request.clone(), token, timeout, cancel)
                .await
            {
                Ok(stream) => return Ok(stream),
                Err(error) => {
                    if !retryable(&error) || !policy.wait_after(attempt, cancel).await? {
                        return Err(error);
                    }
                    attempt += 1;
                }
            }
        }
    }
}

fn retryable(error: &Failure) -> bool {
    matches!(
        error.kind(),
        FailureKind::Deadline | FailureKind::Rpc(ErrorCode::Unavailable)
    ) && matches!(error.authorization_termination(), Ok(None))
}

#[cfg(test)]
mod tests {
    use super::*;
    use connectrpc::{ConnectError, ErrorDetail};
    use home_wire_contract::home::auth::v1::AuthorizationTermination;

    #[test]
    fn authority_termination_never_retries_a_cached_cursor_even_when_unavailable() {
        let plain = Failure::from(ConnectError::unavailable("network"));
        assert!(retryable(&plain));
        for value in [Some("CAU".into()), None, Some("%%%".into())] {
            let failure = Failure::from(ConnectError::unavailable("authority").with_detail(
                ErrorDetail {
                    type_url: AuthorizationTermination::TYPE_URL.into(),
                    value,
                    debug: None,
                },
            ));
            assert!(!retryable(&failure));
        }
        assert!(!retryable(&Failure::from(ConnectError::unauthenticated(
            "expired"
        ))));
    }
}

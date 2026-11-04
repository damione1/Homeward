//! Shared authenticated call options on the verified connection.
use crate::Failure;
use connectrpc::client::CallOptions;
use http::HeaderValue;
use std::time::Duration;

/// An in-memory credential. Session coordination and secure persistence belong to NHF-003.
#[derive(Clone)]
pub struct AccessToken(HeaderValue);

impl AccessToken {
    pub fn parse(token: &str) -> Result<Self, Failure> {
        if token.is_empty() {
            return Err(Failure::invalid_configuration());
        }
        let mut header = HeaderValue::from_str(&format!("Bearer {token}"))
            .map_err(|_| Failure::invalid_configuration())?;
        header.set_sensitive(true);
        Ok(Self(header))
    }
}

impl AccessToken {
    pub(crate) fn options(&self, timeout: Duration) -> Result<CallOptions, Failure> {
        CallOptions::default()
            .with_timeout(timeout)
            .try_with_header(http::header::AUTHORIZATION, self.0.clone())
            .map_err(Failure::from)
    }
}

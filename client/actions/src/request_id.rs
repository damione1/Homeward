//! A fresh key belongs to exactly one explicit intention; it never authorizes replay.
use crate::IdentityFailure;
use home_client_model::api::RequestId;
pub fn new_request_id() -> Result<RequestId, IdentityFailure> {
    let mut bytes = [0u8; 16];
    getrandom::fill(&mut bytes).map_err(|_| IdentityFailure::RandomUnavailable)?;
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    let mut text = String::with_capacity(36);
    for (index, byte) in bytes.iter().enumerate() {
        if matches!(index, 4 | 6 | 8 | 10) {
            text.push('-');
        }
        use std::fmt::Write;
        write!(&mut text, "{byte:02x}").map_err(|_| IdentityFailure::Invalid)?;
    }
    RequestId::try_from(text.as_str()).map_err(|_| IdentityFailure::Invalid)
}

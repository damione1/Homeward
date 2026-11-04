//! Admission of the standard Google operation envelope. Domain adapters then
//! decode only their declared metadata/result types; this grants no authority.
use crate::{api_errors::admit_status, google::longrunning as wire};
use buffa::Message;
use buffa_types::google::protobuf::Any;
use home_client_model::api::OperationName;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InvalidOperation {
    Name,
    Limit,
    Unsupported,
    Inconsistent,
    Malformed,
}

fn admitted_any(value: &Any) -> bool {
    !value.type_url.is_empty()
        && value.type_url.len() <= 256
        && value.__buffa_unknown_fields.is_empty()
}

pub fn admit_envelope(
    raw: &wire::Operation,
    expected: &OperationName,
) -> Result<(), InvalidOperation> {
    if raw.name != expected.as_str() {
        return Err(InvalidOperation::Name);
    }
    if raw.compute_size(&mut buffa::SizeCache::default()) > 1024 * 1024
        || raw.metadata.compute_size(&mut buffa::SizeCache::default()) > 64 * 1024
    {
        return Err(InvalidOperation::Limit);
    }
    if !raw.__buffa_unknown_fields.is_empty()
        || !raw.metadata.is_set()
        || !admitted_any(&raw.metadata)
    {
        return Err(InvalidOperation::Unsupported);
    }
    match (&raw.result, raw.done) {
        (None, false) => Ok(()),
        (Some(wire::operation::Result::Response(value)), true) if admitted_any(value) => Ok(()),
        (Some(wire::operation::Result::Error(value)), true) => admit_status(value)
            .map(|_| ())
            .map_err(|_| InvalidOperation::Malformed),
        (Some(wire::operation::Result::Response(_)), true) => Err(InvalidOperation::Unsupported),
        (None, true)
        | (Some(wire::operation::Result::Error(_)), false)
        | (Some(wire::operation::Result::Response(_)), false) => {
            Err(InvalidOperation::Inconsistent)
        }
    }
}

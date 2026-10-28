use super::*;
use crate::administration_host::read::{ReadError, TIMEOUT, call};
use home_client_session::{AccessLease, SessionCoordinator};
use home_client_transport::Connection;
use home_wire_contract::logic_contract::{self as contract, Request, Response};
use std::collections::BTreeSet;
use tokio_util::sync::CancellationToken;

fn admit(response: Response<'_>) -> Result<(), ReadError> {
    contract::admit_response(response).map_err(|_| ReadError::Invalid)
}
fn cursor(
    value: &wire::Continuation,
    seen: &mut BTreeSet<String>,
) -> Result<Option<String>, ReadError> {
    match &value.result {
        Some(wire::continuation::Result::NextPageToken(value)) => {
            if !seen.insert(value.clone()) {
                return Err(ReadError::Invalid);
            }
            Ok(Some(value.clone()))
        }
        Some(wire::continuation::Result::Complete(_)) => Ok(None),
        None => Err(ReadError::Invalid),
    }
}
async fn catalogue(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    cancel: &CancellationToken,
) -> Result<Catalogue, ReadError> {
    let mut definitions = Vec::new();
    let mut token = None;
    let mut seen = BTreeSet::new();
    let mut ids = BTreeSet::new();
    let mut captured: Option<(Context, u32, u32)> = None;
    let mut bytes = 0;
    loop {
        let request = wire::ListDefinitionsRequest {
            page_size: 10,
            page_token: token,
            ..Default::default()
        };
        contract::admit_request(Request::Definitions(&request)).map_err(|_| ReadError::Invalid)?;
        let response = call!(
            session,
            lease,
            cancel,
            connection.logic(),
            list_definitions,
            request
        );
        admit(Response::Definitions(&response))?;
        let context = Context::capture(&response.context)?;
        if captured.as_ref().is_some_and(|value| {
            value
                != &(
                    context.clone(),
                    response.retained_count,
                    response.reported_count,
                )
        }) {
            return Err(ReadError::Invalid);
        }
        if response.retained_count > 256 {
            return Err(ReadError::Invalid);
        }
        captured = Some((context, response.retained_count, response.reported_count));
        bytes += buffa::Message::compute_size(&response, &mut Default::default());
        if bytes > contract::MAX_BYTES {
            return Err(ReadError::Invalid);
        }
        token = cursor(&response.continuation, &mut seen)?;
        for definition in response.definitions {
            if !ids.insert(definition.reference.id.clone()) {
                return Err(ReadError::Invalid);
            }
            definitions.push(definition);
        }
        if definitions.len() > 256 {
            return Err(ReadError::Invalid);
        }
        if token.is_none() {
            break;
        }
    }
    let (context, retained, reported) = captured.ok_or(ReadError::Invalid)?;
    if definitions.len() != retained as usize {
        return Err(ReadError::Invalid);
    }
    Ok(Catalogue {
        context,
        definitions,
        retained,
        reported,
    })
}
pub(in crate::administration_host) async fn load(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    selection: Selection,
    cancel: &CancellationToken,
) -> Result<Payload, ReadError> {
    match selection {
        Selection::Catalogue => catalogue(session, connection, lease, cancel)
            .await
            .map(Payload::Catalogue),
        Selection::Definition { context, id } => {
            let catalogue = catalogue(session, connection, lease, cancel).await?;
            if catalogue.context != context {
                return Err(ReadError::Invalid);
            }
            let definition = catalogue
                .definitions
                .iter()
                .find(|value| value.reference.id == id.as_str())
                .cloned()
                .ok_or(ReadError::Invalid)?;
            let mut runs = Vec::new();
            let mut token = None;
            let mut seen = BTreeSet::new();
            let mut ids = BTreeSet::new();
            let mut bytes = 0;
            loop {
                let request = wire::ListRunsRequest {
                    context: context.to_wire().into(),
                    definition_id: id.as_str().into(),
                    page_size: 32,
                    page_token: token,
                    ..Default::default()
                };
                contract::admit_request(Request::Runs(&request)).map_err(|_| ReadError::Invalid)?;
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.logic(),
                    list_runs,
                    request
                );
                admit(Response::Runs(&response))?;
                if Context::capture(&response.context)? != context {
                    return Err(ReadError::Invalid);
                }
                bytes += buffa::Message::compute_size(&response, &mut Default::default());
                if bytes > contract::MAX_BYTES {
                    return Err(ReadError::Invalid);
                }
                token = cursor(&response.continuation, &mut seen)?;
                for run in response.runs {
                    if run.definition.id != id.as_str() || !ids.insert(run.id.clone()) {
                        return Err(ReadError::Invalid);
                    }
                    runs.push(run);
                }
                if runs.len() > 64 {
                    return Err(ReadError::Invalid);
                }
                if token.is_none() {
                    break;
                }
            }
            Ok(Payload::Definition {
                catalogue,
                definition: Box::new(definition),
                runs,
            })
        }
        Selection::Run { context, id } => {
            let request = wire::GetRunRequest {
                context: context.to_wire().into(),
                selector: Some(wire::get_run_request::Selector::RunId(id.as_str().into())),
                ..Default::default()
            };
            contract::admit_request(Request::Get(&request)).map_err(|_| ReadError::Invalid)?;
            let response = call!(session, lease, cancel, connection.logic(), get_run, request);
            admit(Response::Get(&response))?;
            if Context::capture(&response.context)? != context || response.run.id != id.as_str() {
                return Err(ReadError::Invalid);
            }
            Ok(Payload::Run {
                context,
                run: Box::new(response.run.into_option().ok_or(ReadError::Invalid)?),
            })
        }
        Selection::Trace { context, id } => {
            let mut rows: Vec<wire::TraceRow> = Vec::new();
            let mut token = None;
            let mut seen = BTreeSet::new();
            let mut captured: Option<(String, wire::Run)> = None;
            let mut bytes = 0;
            loop {
                let request = wire::ReadTraceRequest {
                    context: context.to_wire().into(),
                    run_id: id.as_str().into(),
                    page_size: 100,
                    page_token: token,
                    ..Default::default()
                };
                contract::admit_request(Request::Trace(&request))
                    .map_err(|_| ReadError::Invalid)?;
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.logic(),
                    read_trace,
                    request
                );
                admit(Response::Trace(&response))?;
                if Context::capture(&response.context)? != context
                    || response.run.id != id.as_str()
                    || captured.as_ref().is_some_and(|(capture, run)| {
                        *capture != response.capture_id || Some(run) != response.run.as_option()
                    })
                {
                    return Err(ReadError::Invalid);
                }
                bytes += buffa::Message::compute_size(&response, &mut Default::default());
                if bytes > contract::MAX_BYTES {
                    return Err(ReadError::Invalid);
                }
                if let (Some(previous), Some(next)) = (rows.last(), response.rows.first())
                    && (next.ordinal <= previous.ordinal
                        || contract::next_ordinal(previous)
                            .is_some_and(|expected| expected != next.ordinal))
                {
                    return Err(ReadError::Invalid);
                }
                token = cursor(&response.continuation, &mut seen)?;
                captured = Some((
                    response.capture_id,
                    response.run.into_option().ok_or(ReadError::Invalid)?,
                ));
                rows.extend(response.rows);
                if rows.len() > contract::MAX_CAPTURE_ROWS {
                    return Err(ReadError::Invalid);
                }
                if token.is_none() {
                    break;
                }
            }
            let (capture, run) = captured.ok_or(ReadError::Invalid)?;
            Ok(Payload::Trace {
                context,
                capture,
                run: Box::new(run),
                rows,
            })
        }
    }
}

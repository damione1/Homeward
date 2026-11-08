//! Configuration metadata reuses the settings session owner and its private purge.
mod planner;
mod watch;
mod workflow;
use std::collections::BTreeSet;
pub(super) use watch::{Permit as WatchPermit, Stop as WatchStop, run as watch};
pub(super) use workflow::{Completion, Workflow, execute};

use buffa::EnumValue;
use home_client_session::{AccessLease, SessionCoordinator};
use home_client_transport::Connection;
use home_wire_contract::{
    configuration_contract::{self, ConfigurationResponse},
    home::api::v2 as wire,
};
use tokio_util::sync::CancellationToken;

use super::{
    read::{PageToken, ReadError, TIMEOUT, call},
    render,
};
use crate::configuration::{Capture, DocumentName as DocumentId, Workspace};

fn workspace_label(value: &Workspace) -> String {
    match value {
        Workspace::Observed { hash, dirty } => {
            format!("{} · {}", dirty.label(), render::hex(hash.bytes()))
        }
        Workspace::Unobserved(reason) => reason.label().into(),
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) enum Selection {
    Documents,
    Document(DocumentId),
    Versions(DocumentId),
    Schemas,
    Generations,
}
#[derive(Clone)]
pub(super) struct Payload {
    pub root: Capture,
    pub page: Page,
}
#[derive(Clone)]
pub(super) enum Page {
    Documents(wire::ListConfigurationDocumentsResponse),
    Document(Box<Capture>),
    Versions {
        id: DocumentId,
        response: wire::ListConfigurationRevisionsResponse,
    },
    Schemas(wire::ListConfigurationSchemasResponse),
    Generations(wire::ListConfigurationGenerationsResponse),
}
pub(super) async fn load(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    selection: Selection,
    page: Option<PageToken>,
    cancel: &CancellationToken,
) -> Result<Payload, ReadError> {
    let parent = home_name(session, connection, lease, cancel).await?;
    let page_token = page
        .map(|value| value.as_str().to_string())
        .unwrap_or_default();
    let api = connection.configuration();
    let root = operational_capture(session, connection, lease, cancel).await?;
    match selection {
        Selection::Documents => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_documents,
                wire::ListConfigurationDocumentsRequest {
                    parent: parent.clone(),
                    page_size: 50,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(ConfigurationResponse::Documents(
                &response,
            ))
            .map_err(|_| ReadError::Invalid)?;
            let mut ids = BTreeSet::new();
            if response
                .configuration_documents
                .iter()
                .any(|value| !ids.insert(value.id.value.as_str()))
            {
                return Err(ReadError::Invalid);
            }
            Ok(Page::Documents(response))
        }
        Selection::Document(id) => {
            let capture = if id == *root.id() {
                root.clone()
            } else {
                let response = call!(
                    session,
                    lease,
                    cancel,
                    api,
                    read_document,
                    wire::GetConfigurationDocumentRequest {
                        name: id.as_str().into(),
                        ..Default::default()
                    }
                );
                Capture::admit(response).map_err(|_| ReadError::Invalid)?
            };
            if capture.id() != &id {
                return Err(ReadError::Invalid);
            }
            Ok(Page::Document(Box::new(capture)))
        }
        Selection::Versions(id) => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_versions,
                wire::ListConfigurationRevisionsRequest {
                    parent: id.as_str().into(),
                    page_size: 50,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(ConfigurationResponse::Versions(
                &response,
            ))
            .map_err(|_| ReadError::Invalid)?;
            let mut revisions = BTreeSet::new();
            if response.configuration_revisions.iter().any(|value| {
                value.id.value != id.as_str() || !revisions.insert(value.version.revision)
            }) {
                return Err(ReadError::Invalid);
            }
            Ok(Page::Versions { id, response })
        }
        Selection::Schemas => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_schemas,
                wire::ListConfigurationSchemasRequest {
                    parent: parent.clone(),
                    page_size: 50,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(ConfigurationResponse::Schemas(
                &response,
            ))
            .map_err(|_| ReadError::Invalid)?;
            let mut keys = BTreeSet::new();
            if response.configuration_schemas.iter().any(|value| {
                !keys.insert((value.schema_version.as_str(), value.syntax_profile.as_str()))
            }) {
                return Err(ReadError::Invalid);
            }
            Ok(Page::Schemas(response))
        }
        Selection::Generations => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_generations,
                wire::ListConfigurationGenerationsRequest {
                    parent: parent.clone(),
                    page_size: 50,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(
                ConfigurationResponse::Generations(&response),
            )
            .map_err(|_| ReadError::Invalid)?;
            let mut revisions = BTreeSet::new();
            if response
                .configuration_generations
                .iter()
                .any(|value| !revisions.insert(value.generation.revision))
            {
                return Err(ReadError::Invalid);
            }
            Ok(Page::Generations(response))
        }
    }
    .map(|page| Payload { root, page })
}

fn next(value: &str) -> Option<PageToken> {
    if value.is_empty() {
        None
    } else {
        Some(PageToken::configuration(value).expect("admitted configuration cursor"))
    }
}

pub(super) fn rows(payload: &Payload) -> (Vec<render::Row>, Option<PageToken>) {
    let mut rows = Vec::new();
    let mut push = |target, title: String, detail: String| {
        rows.push(render::Row {
            token: i32::try_from(rows.len() + 1).expect("bounded configuration rows"),
            target,
            title,
            detail,
        })
    };
    let navigate = |value| render::Target::Configuration(value);
    let mut page = None;
    match &payload.page {
        Page::Documents(value) => {
            push(
                navigate(Selection::Schemas),
                "Schemas".into(),
                "Formats and write scope exposed by the server".into(),
            );
            push(
                navigate(Selection::Generations),
                "Generations".into(),
                "Publication History and Restore Availability".into(),
            );
            for value in &value.configuration_documents {
                let id = DocumentId::parse(&value.name).expect("admitted document");
                push(
                    navigate(Selection::Document(id)),
                    value.id.value.clone(),
                    format!(
                        "SQL version {} · {}",
                        value.version.revision, value.schema_version
                    ),
                );
            }
            page = next(&value.next_page_token);
        }
        Page::Document(value) => {
            push(
                navigate(Selection::Documents),
                "Documents".into(),
                "Back to Catalogue".into(),
            );
            push(
                navigate(Selection::Versions(value.id().clone())),
                "Retained Versions".into(),
                "View This Document’s History".into(),
            );
            push(
                render::fact("configuration/sql"),
                format!("SQL document · version {}", value.sql().revision()),
                render::hex(value.sql().source().bytes()),
            );
            push(
                render::fact("configuration/active"),
                format!(
                    "Active configuration · generation {}",
                    value.active().revision()
                ),
                render::hex(value.active().manifest().bytes()),
            );
            match value.workspace() {
                Workspace::Observed { hash, dirty } => push(
                    render::fact("configuration/workspace"),
                    dirty.label().into(),
                    format!("Observed files · {}", render::hex(hash.bytes())),
                ),
                Workspace::Unobserved(reason) => push(
                    render::fact("configuration/workspace"),
                    "Unobserved Files".into(),
                    reason.label().into(),
                ),
            }
            push(
                render::fact("configuration/source"),
                "Protected Source".into(),
                "This document contains metadata. Editing requires an explicitly provided source."
                    .into(),
            );
            push(
                render::fact("configuration/sql-active"),
                "SQL Document Usage".into(),
                if value.document().active {
                    "This version participates in the active generation"
                } else {
                    "Saved version separate from the active generation"
                }
                .into(),
            );
        }
        Page::Versions { id, response } => {
            push(
                navigate(Selection::Document(id.clone())),
                "Current Document".into(),
                id.as_str().into(),
            );
            for value in &response.configuration_revisions {
                let detail = match value.availability {
                    EnumValue::Known(wire::VersionAvailability::Retained) => format!(
                        "Retained · {} · {}",
                        value.schema_version,
                        render::at(&value.update_time)
                    ),
                    EnumValue::Known(wire::VersionAvailability::Purged) => {
                        "Source purged · no date or schema version inferred".into()
                    }
                    EnumValue::Known(wire::VersionAvailability::Unspecified)
                    | EnumValue::Unknown(_) => "Unsupported availability".into(),
                };
                push(
                    render::fact(format!("configuration/version/{}", value.version.revision)),
                    format!("SQL version {}", value.version.revision),
                    format!("{detail}\n{}", render::hex(&value.version.source_sha256)),
                );
            }
            page = next(&response.next_page_token);
        }
        Page::Schemas(value) => {
            push(
                navigate(Selection::Documents),
                "Documents".into(),
                "Back to Catalogue".into(),
            );
            for (index, value) in value.configuration_schemas.iter().enumerate() {
                push(
                    render::fact(format!("configuration/schema/{index}")),
                    value.schema_version.clone(),
                    format!(
                        "{} · {}",
                        value.syntax_profile,
                        if value.editable_operational_workspace {
                            "Explicitly Provided Operational Source"
                        } else {
                            "Specific authorization · no global writes"
                        }
                    ),
                );
            }
            page = next(&value.next_page_token);
        }
        Page::Generations(value) => {
            push(
                navigate(Selection::Documents),
                "Documents".into(),
                "Back to Catalogue".into(),
            );
            for value in &value.configuration_generations {
                let availability = match value.availability {
                    EnumValue::Known(wire::GenerationAvailability::Complete) => "Complete History",
                    EnumValue::Known(wire::GenerationAvailability::PartiallyPurged) => {
                        "History partially purged"
                    }
                    EnumValue::Known(wire::GenerationAvailability::Unspecified)
                    | EnumValue::Unknown(_) => "Unsupported availability",
                };
                push(
                    render::fact(format!(
                        "configuration/generation/{}",
                        value.generation.revision
                    )),
                    format!("Generation {}", value.generation.revision),
                    format!(
                        "{availability} · {}\n{}",
                        render::at(&value.create_time),
                        render::hex(&value.generation.manifest_sha256)
                    ),
                );
            }
            page = next(&value.next_page_token);
        }
    }
    (rows, page)
}

pub(super) async fn home_name(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    cancel: &CancellationToken,
) -> Result<String, ReadError> {
    let homes = call!(
        session,
        lease,
        cancel,
        connection.home(),
        list_homes,
        wire::ListHomesRequest {
            page_size: 1,
            ..Default::default()
        }
    );
    if homes.homes.len() != 1 || !homes.next_page_token.is_empty() {
        return Err(ReadError::Invalid);
    }
    let name = homes.homes[0].name.clone();
    if !configuration_contract::home_name(&name) {
        return Err(ReadError::Invalid);
    }
    Ok(name)
}
pub(super) async fn operational_capture(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    cancel: &CancellationToken,
) -> Result<Capture, ReadError> {
    let parent = home_name(session, connection, lease, cancel).await?;
    let mut token = String::new();
    let mut found = None;
    let mut seen = BTreeSet::new();
    for _ in 0..32 {
        let page = call!(
            session,
            lease,
            cancel,
            connection.configuration(),
            list_documents,
            wire::ListConfigurationDocumentsRequest {
                parent: parent.clone(),
                page_size: 100,
                page_token: token,
                ..Default::default()
            }
        );
        configuration_contract::admit_configuration_response(ConfigurationResponse::Documents(
            &page,
        ))
        .map_err(|_| ReadError::Invalid)?;
        for document in page.configuration_documents {
            if !seen.insert(document.name.clone()) || seen.len() > crate::administration::MAX_ROWS {
                return Err(ReadError::Invalid);
            }
            if document.kind == wire::DocumentKind::OperationalYaml
                && document.active
                && found.replace(document.name).is_some()
            {
                return Err(ReadError::Invalid);
            }
        }
        if page.next_page_token.is_empty() {
            let name = found.ok_or(ReadError::Invalid)?;
            let document = call!(
                session,
                lease,
                cancel,
                connection.configuration(),
                read_document,
                wire::GetConfigurationDocumentRequest {
                    name,
                    ..Default::default()
                }
            );
            return Capture::admit(document).map_err(|_| ReadError::Invalid);
        }
        token = page.next_page_token;
    }
    Err(ReadError::Invalid)
}

pub(super) async fn source_document_name(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    cancel: &CancellationToken,
    native_key: &str,
) -> Result<String, ReadError> {
    let parent = home_name(session, connection, lease, cancel).await?;
    let mut token = String::new();
    let mut seen = BTreeSet::new();
    for _ in 0..32 {
        let page = call!(
            session,
            lease,
            cancel,
            connection.configuration(),
            list_documents,
            wire::ListConfigurationDocumentsRequest {
                parent: parent.clone(),
                page_size: 100,
                page_token: token,
                ..Default::default()
            }
        );
        configuration_contract::admit_configuration_response(ConfigurationResponse::Documents(
            &page,
        ))
        .map_err(|_| ReadError::Invalid)?;
        for document in page.configuration_documents {
            if !seen.insert(document.name.clone()) || seen.len() > crate::administration::MAX_ROWS {
                return Err(ReadError::Invalid);
            }
            if document.id.value == native_key {
                return Ok(document.name);
            }
        }
        if page.next_page_token.is_empty() {
            return Err(ReadError::Invalid);
        }
        token = page.next_page_token;
    }
    Err(ReadError::Invalid)
}

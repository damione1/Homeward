//! Configuration metadata reuses the settings session owner and its private purge.
use std::collections::BTreeSet;

use buffa::EnumValue;
use home_client_session::{AccessLease, SessionCoordinator};
use home_client_transport::Connection;
use home_wire_contract::{
    configuration_contract::{self, ConfigurationResponse},
    home::configuration::v1 as wire,
};
use tokio_util::sync::CancellationToken;

use super::{
    read::{PageToken, ReadError, TIMEOUT, call},
    render,
};
use crate::configuration::{Capture, DocumentId, UnobservedReason, Workspace};

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) enum Selection {
    Documents,
    Document(DocumentId),
    Versions(DocumentId),
    Schemas,
    Generations,
}
#[derive(Clone)]
pub(super) enum Payload {
    Documents(wire::ListDocumentsResponse),
    Document(Box<Capture>),
    Versions {
        id: DocumentId,
        response: wire::ListVersionsResponse,
    },
    Schemas(wire::ListSchemasResponse),
    Generations(wire::ListGenerationsResponse),
}
pub(super) async fn load(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    selection: Selection,
    page: Option<PageToken>,
    cancel: &CancellationToken,
) -> Result<Payload, ReadError> {
    let page = wire::PageRequest {
        page_size: 50,
        page_token: page.map(|value| value.as_str().into()),
        ..Default::default()
    };
    let api = connection.configuration();
    match selection {
        Selection::Documents => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_documents,
                wire::ListDocumentsRequest {
                    page: page.into(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(ConfigurationResponse::Documents(
                &response,
            ))
            .map_err(|_| ReadError::Invalid)?;
            let mut ids = BTreeSet::new();
            if response
                .documents
                .iter()
                .any(|value| !ids.insert(value.id.value.as_str()))
            {
                return Err(ReadError::Invalid);
            }
            Ok(Payload::Documents(response))
        }
        Selection::Document(id) => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                read_document,
                wire::ReadDocumentRequest {
                    id: id.to_wire().into(),
                    ..Default::default()
                }
            );
            let capture = Capture::admit(response).map_err(|_| ReadError::Invalid)?;
            if capture.id() != &id {
                return Err(ReadError::Invalid);
            }
            Ok(Payload::Document(Box::new(capture)))
        }
        Selection::Versions(id) => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_versions,
                wire::ListVersionsRequest {
                    id: id.to_wire().into(),
                    page: page.into(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(ConfigurationResponse::Versions(
                &response,
            ))
            .map_err(|_| ReadError::Invalid)?;
            let mut revisions = BTreeSet::new();
            if response.versions.iter().any(|value| {
                value.id.value != id.as_str() || !revisions.insert(value.version.revision)
            }) {
                return Err(ReadError::Invalid);
            }
            Ok(Payload::Versions { id, response })
        }
        Selection::Schemas => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_schemas,
                wire::ListSchemasRequest {
                    page: page.into(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(ConfigurationResponse::Schemas(
                &response,
            ))
            .map_err(|_| ReadError::Invalid)?;
            let mut keys = BTreeSet::new();
            if response.schemas.iter().any(|value| {
                !keys.insert((value.schema_version.as_str(), value.syntax_profile.as_str()))
            }) {
                return Err(ReadError::Invalid);
            }
            Ok(Payload::Schemas(response))
        }
        Selection::Generations => {
            let response = call!(
                session,
                lease,
                cancel,
                api,
                list_generations,
                wire::ListGenerationsRequest {
                    page: page.into(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(
                ConfigurationResponse::Generations(&response),
            )
            .map_err(|_| ReadError::Invalid)?;
            let mut revisions = BTreeSet::new();
            if response
                .generations
                .iter()
                .any(|value| !revisions.insert(value.generation.revision))
            {
                return Err(ReadError::Invalid);
            }
            Ok(Payload::Generations(response))
        }
    }
}

fn next(value: &wire::PageContinuation) -> Option<PageToken> {
    match &value.result {
        Some(wire::page_continuation::Result::NextPageToken(value)) => {
            Some(PageToken::configuration(value).expect("admitted configuration cursor"))
        }
        Some(wire::page_continuation::Result::Complete(_)) | None => None,
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
    match payload {
        Payload::Documents(value) => {
            push(
                navigate(Selection::Schemas),
                "Schémas".into(),
                "Formats et périmètre d’écriture exposés par le serveur".into(),
            );
            push(
                navigate(Selection::Generations),
                "Générations".into(),
                "Historique des publications et disponibilité de restauration".into(),
            );
            for value in &value.documents {
                let id = DocumentId::parse(&value.id.value).expect("admitted document");
                push(
                    navigate(Selection::Document(id)),
                    value.id.value.clone(),
                    format!(
                        "Version SQL {} · {}",
                        value.version.revision, value.schema_version
                    ),
                );
            }
            page = next(&value.continuation);
        }
        Payload::Document(value) => {
            push(
                navigate(Selection::Documents),
                "Documents".into(),
                "Revenir au catalogue".into(),
            );
            push(
                navigate(Selection::Versions(value.id().clone())),
                "Versions conservées".into(),
                "Consulter l’historique de ce document".into(),
            );
            push(
                render::fact("configuration/sql"),
                format!("Document SQL · version {}", value.sql().revision()),
                render::hex(value.sql().source().bytes()),
            );
            push(
                render::fact("configuration/active"),
                format!(
                    "Configuration active · génération {}",
                    value.active().revision()
                ),
                render::hex(value.active().manifest().bytes()),
            );
            match value.workspace() {
                Workspace::Observed { hash, dirty } => push(
                    render::fact("configuration/workspace"),
                    dirty.label().into(),
                    format!("Fichiers observés · {}", render::hex(hash.bytes())),
                ),
                Workspace::Unobserved(reason) => push(
                    render::fact("configuration/workspace"),
                    "Fichiers non observés".into(),
                    match reason {
                        UnobservedReason::NoPhysicalLocator => {
                            "Aucun emplacement de fichier disponible"
                        }
                        UnobservedReason::SourceUnavailable => "La source est indisponible",
                    }
                    .into(),
                ),
            }
            push(render::fact("configuration/source"),"Source protégée".into(),"Ce document fournit des métadonnées. L’édition exige une source explicitement fournie.".into());
        }
        Payload::Versions { id, response } => {
            push(
                navigate(Selection::Document(id.clone())),
                "Document courant".into(),
                id.as_str().into(),
            );
            for value in &response.versions {
                let detail = match value.availability {
                    EnumValue::Known(wire::VersionAvailability::Retained) => format!(
                        "Conservée · {} · {}",
                        value.schema_version,
                        render::at(&value.updated_at)
                    ),
                    EnumValue::Known(wire::VersionAvailability::Purged) => {
                        "Source purgée · aucune date ou version de schéma déduite".into()
                    }
                    EnumValue::Known(wire::VersionAvailability::Unspecified)
                    | EnumValue::Unknown(_) => "Disponibilité non prise en charge".into(),
                };
                push(
                    render::fact(format!("configuration/version/{}", value.version.revision)),
                    format!("Version SQL {}", value.version.revision),
                    format!("{detail}\n{}", render::hex(&value.version.source_sha256)),
                );
            }
            page = next(&response.continuation);
        }
        Payload::Schemas(value) => {
            push(
                navigate(Selection::Documents),
                "Documents".into(),
                "Revenir au catalogue".into(),
            );
            for (index, value) in value.schemas.iter().enumerate() {
                push(
                    render::fact(format!("configuration/schema/{index}")),
                    value.schema_version.clone(),
                    format!(
                        "{} · {}",
                        value.syntax_profile,
                        if value.editable_operational_workspace {
                            "Source opérationnelle fournie explicitement"
                        } else {
                            "Autorité spécifique · pas d’écriture globale"
                        }
                    ),
                );
            }
            page = next(&value.continuation);
        }
        Payload::Generations(value) => {
            push(
                navigate(Selection::Documents),
                "Documents".into(),
                "Revenir au catalogue".into(),
            );
            for value in &value.generations {
                let availability = match value.availability {
                    EnumValue::Known(wire::GenerationAvailability::Complete) => {
                        "Historique complet"
                    }
                    EnumValue::Known(wire::GenerationAvailability::PartiallyPurged) => {
                        "Historique partiellement purgé"
                    }
                    EnumValue::Known(wire::GenerationAvailability::Unspecified)
                    | EnumValue::Unknown(_) => "Disponibilité non prise en charge",
                };
                push(
                    render::fact(format!(
                        "configuration/generation/{}",
                        value.generation.revision
                    )),
                    format!("Génération {}", value.generation.revision),
                    format!(
                        "{availability} · {}\n{}",
                        render::at(&value.created_at),
                        render::hex(&value.generation.manifest_sha256)
                    ),
                );
            }
            page = next(&value.continuation);
        }
    }
    (rows, page)
}

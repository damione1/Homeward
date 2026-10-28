//! Synthetic logic catalogue and captures use the same private session owner.
//! No routine, template, device route or retry queue lives in this module.
mod read;
mod render;
mod workflow;
use home_wire_contract::{
    home::{configuration::v1 as cfg, logic::v1 as wire},
    logic_contract::{DefinitionId, RunId},
};
pub(super) use read::load;
pub(super) use render::rows;
pub(super) use workflow::{Completion, Workflow, execute};

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct Context {
    epoch: String,
    catalogue: u64,
    generation: i64,
    manifest: [u8; 32],
}
impl Context {
    fn capture(value: &wire::Context) -> Result<Self, super::read::ReadError> {
        Ok(Self {
            epoch: value.epoch.clone(),
            catalogue: value.catalog_revision,
            generation: value.generation.revision,
            manifest: value
                .generation
                .manifest_sha256
                .as_slice()
                .try_into()
                .map_err(|_| super::read::ReadError::Invalid)?,
        })
    }
    fn to_wire(&self) -> wire::Context {
        wire::Context {
            deployment: wire::Deployment::SyntheticOnly.into(),
            epoch: self.epoch.clone(),
            catalog_revision: self.catalogue,
            generation: cfg::ActiveGeneration {
                revision: self.generation,
                manifest_sha256: self.manifest.to_vec(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) enum Selection {
    Catalogue,
    Definition { context: Context, id: DefinitionId },
    Run { context: Context, id: RunId },
    Trace { context: Context, id: RunId },
}
#[derive(Clone)]
pub(super) struct Catalogue {
    context: Context,
    definitions: Vec<wire::Definition>,
    retained: u32,
    reported: u32,
}
#[derive(Clone)]
pub(super) enum Payload {
    Catalogue(Catalogue),
    Definition {
        catalogue: Catalogue,
        definition: Box<wire::Definition>,
        runs: Vec<wire::Run>,
    },
    Run {
        context: Context,
        run: Box<wire::Run>,
    },
    Trace {
        context: Context,
        capture: String,
        run: Box<wire::Run>,
        rows: Vec<wire::TraceRow>,
    },
}

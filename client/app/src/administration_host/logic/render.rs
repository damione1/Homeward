use super::*;
use crate::administration_host::render::{self, Row, Target};
use buffa::EnumValue;
use home_wire_contract::home::{
    configurationplan::v1 as plan, history::v1 as history, observability::v1 as obs,
};

macro_rules! label {
    ($fn:ident,$ty:path,$($variant:ident => $text:literal),+ $(,)?) => {
        fn $fn(value:EnumValue<$ty>) -> &'static str {
            use $ty as V;
            match value { $(EnumValue::Known(V::$variant)=>$text,)+
                EnumValue::Known(V::Unspecified)|EnumValue::Unknown(_)=>"Unsupported value" }
        }
    }
}
label!(mode,wire::RunMode,Single=>"Single",Restart=>"Restart",Queued=>"Queued",Parallel=>"Parallel");
label!(status,wire::RunStatus,Queued=>"Pending",Running=>"In progress",Completed=>"Done",Canceled=>"Cancelled",Interrupted=>"Interrupted",Failed=>"Rejected or failed");
label!(reason,wire::RunReason,ExplicitRequest=>"Explicit Request",SingleBusy=>"Single-run slot already occupied",RestartReplaced=>"Replaced by a new run",WaitingForSlot=>"Waiting for previous step",ExplicitCancellation=>"Future steps cancelled",ServerRestart=>"Server Restart",SyntheticFailure=>"Synthetic error");
label!(compatibility,wire::Compatibility,Supported=>"Support",Partial=>"Partial",Unsupported=>"Unsupported");
label!(diagnostic,wire::Diagnostic,InvalidSchema=>"Invalid schema",ExcludedReference=>"Excluded Reference",UnsupportedExtension=>"Unsupported extension",SourceUnavailable=>"Source unavailable");
label!(gap,wire::GapReason,Evicted=>"Evicted Events",Redacted=>"Redacted Events",NotRetained=>"Unretained Events");
label!(action,obs::TraceActionKind,Variables=>"Variables",Sequence=>"Sequence",Choose=>"Choose",If=>"Condition",Repeat=>"Repeat",Parallel=>"Parallel",Wait=>"Wait",Stop=>"Off",Call=>"Call",Delay=>"Delay",WaitTemplate=>"Waiting for template");
label!(stage,obs::TraceNodeStage,Entered=>"Entry",Completed=>"Done",Stopped=>"Stopped",Cancelled=>"Cancelled",Failed=>"Failed",Waiting=>"Pending");
label!(outcome,history::CommandOutcome,Accepted=>"Accepted",Dispatched=>"Dispatched",Acknowledged=>"Acknowledged",Confirmed=>"Confirmed",Indeterminate=>"Uncertain",Refused=>"Rejected",NotAttempted=>"Not attempted");
fn family(value: EnumValue<plan::SourceFamily>) -> &'static str {
    match value {
        EnumValue::Known(plan::SourceFamily::Automation) => "Automation",
        EnumValue::Known(plan::SourceFamily::Script) => "Script",
        EnumValue::Known(plan::SourceFamily::Scene) => "Scene",
        EnumValue::Known(
            plan::SourceFamily::Operational
            | plan::SourceFamily::DeclarativeV1
            | plan::SourceFamily::DeclarativeV2
            | plan::SourceFamily::Authorization
            | plan::SourceFamily::Ownership
            | plan::SourceFamily::Dashboard
            | plan::SourceFamily::Unspecified,
        )
        | EnumValue::Unknown(_) => "Unsupported family",
    }
}
fn actor(run: &wire::Run) -> String {
    match &run.actor.observation {
        Some(wire::actor::Observation::Account(value)) => {
            format!("Recorded actor: {}", value.value)
        }
        Some(wire::actor::Observation::Redacted(_)) => "Actor redacted by the server".into(),
        Some(wire::actor::Observation::NotRecorded(_)) => "Actor not recorded".into(),
        None => "Actor unavailable".into(),
    }
}
fn run_detail(run: &wire::Run) -> String {
    format!(
        "{} · {} · {}\nGeneration {} · {} · {}\n{}\nAttempt {} · No hardware effect validated",
        mode(run.mode),
        status(run.status),
        reason(run.reason),
        run.generation.revision,
        render::hex(&run.generation.manifest_sha256),
        render::at(&run.created_at),
        actor(run),
        run.start_request_id
    )
}
pub(in crate::administration_host) fn rows(
    payload: &Payload,
) -> (
    Vec<Row>,
    Option<crate::administration_host::read::PageToken>,
) {
    let mut rows = Vec::new();
    let mut push = |target: Target, title: String, detail: String| {
        rows.push(Row {
            token: rows.len() as i32 + 1,
            target,
            title,
            detail,
        });
    };
    push(
        render::fact("logic/mock"),
        "Logic · synthetic server".into(),
        "Explicit NHA-163 mock. No real routine runs. Opening this view activates no definition."
            .into(),
    );
    push(
        Target::Logic(Selection::Catalogue),
        "Catalogue".into(),
        "Reload Authorized Definitions From Server".into(),
    );
    match payload {
        Payload::Catalogue(value) => {
            push(
                render::fact("logic/count"),
                format!(
                    "{} retained definitions · {} reported",
                    value.retained, value.reported
                ),
                format!(
                    "Catalogue {} · generation {} · epoch {}",
                    value.context.catalogue, value.context.generation, value.context.epoch
                ),
            );
            for value_definition in &value.definitions {
                push(
                    Target::Logic(Selection::Definition {
                        context: value.context.clone(),
                        id: DefinitionId::parse(&value_definition.reference.id)
                            .expect("admitted definition"),
                    }),
                    value_definition.title.clone(),
                    format!(
                        "{} · version {} · {} · {}",
                        family(value_definition.family),
                        value_definition.reference.version.revision,
                        mode(value_definition.mode),
                        compatibility(value_definition.compatibility)
                    ),
                );
            }
        }
        Payload::Definition {
            catalogue,
            definition,
            runs,
        } => {
            push(
                render::fact("logic/definition"),
                definition.title.clone(),
                format!(
                    "{} · {} · version {}\nHash {}\nSchema {} · {}",
                    family(definition.family),
                    mode(definition.mode),
                    definition.reference.version.revision,
                    render::hex(&definition.reference.version.source_sha256),
                    definition.schema_version,
                    compatibility(definition.compatibility)
                ),
            );
            push(
                render::fact("logic/source"),
                "Protected Source".into(),
                format!(
                    "Document {}\nReference {}\nThis metadata is not an editable source. Simulation uses a source supplied separately in Configuration.",
                    definition.source_document.value,
                    render::hex(&definition.protected_source_reference)
                ),
            );
            for (index, value) in definition.diagnostics.iter().enumerate() {
                push(
                    render::fact(format!("logic/diagnostic/{index}")),
                    "Server Diagnostic".into(),
                    diagnostic(*value).into(),
                );
            }
            for run in runs {
                push(
                    Target::Logic(Selection::Run {
                        context: catalogue.context.clone(),
                        id: RunId::parse(&run.id).expect("admitted run"),
                    }),
                    run.id.clone(),
                    run_detail(run),
                );
            }
            if runs.is_empty() {
                push(
                    render::fact("logic/no-runs"),
                    "No retained run".into(),
                    "This reading does not start an execution.".into(),
                );
            }
        }
        Payload::Run { context, run } => {
            push(render::fact("logic/run"), run.id.clone(), run_detail(run));
            push(
                render::fact("logic/run-definition"),
                format!(
                    "Definition {} · version {}",
                    run.definition.id, run.definition.version.revision
                ),
                render::hex(&run.definition.version.source_sha256),
            );
            push(
                Target::Logic(Selection::Trace {
                    context: context.clone(),
                    id: RunId::parse(&run.id).expect("admitted run"),
                }),
                "Inspect Trace".into(),
                "Paged snapshot with explicit gaps and redaction".into(),
            );
        }
        Payload::Trace {
            context,
            capture,
            run,
            rows: events,
        } => {
            push(
                Target::Logic(Selection::Run {
                    context: context.clone(),
                    id: RunId::parse(&run.id).expect("admitted run"),
                }),
                "Back to Run".into(),
                run_detail(run),
            );
            push(
                render::fact("logic/capture"),
                format!("{} events · snapshot {}", events.len(), capture),
                "Fixed server snapshot; no continuous ordering is inferred across gaps.".into(),
            );
            for row in events {
                let (title, detail) = match &row.event {
                    Some(wire::trace_row::Event::Node(value)) => {
                        let source = match &value.source {
                            Some(wire::node::Source::Position(value)) => format!(
                                "{}:{}:{} · {}",
                                value.file, value.line, value.column, value.path
                            ),
                            Some(wire::node::Source::Redacted(_)) => "Source redacted".into(),
                            Some(wire::node::Source::NotRecorded(_)) => "Source not saved".into(),
                            None => "Source unavailable".into(),
                        };
                        (
                            format!("{} · {}", action(value.kind), stage(value.stage)),
                            source,
                        )
                    }
                    Some(wire::trace_row::Event::Command(value)) => (
                        format!("Synthetic command · {}", outcome(value.outcome)),
                        format!(
                            "{} · No arguments or real devices exposed",
                            value.command_id
                        ),
                    ),
                    Some(wire::trace_row::Event::Gap(value)) => (
                        gap(value.reason).into(),
                        if value.count_unknown {
                            "Quantity unknown · continuity not established".into()
                        } else {
                            format!("{} missing events", value.known_missing_count)
                        },
                    ),
                    Some(wire::trace_row::Event::Lifecycle(value)) => {
                        ("Lifecycle".into(), reason(*value).into())
                    }
                    None => ("Event unavailable".into(), String::new()),
                };
                push(
                    render::fact(format!("logic/trace/{}", row.ordinal)),
                    format!("#{} · {title}", row.ordinal),
                    format!("{}\n{detail}", render::at(&row.at)),
                );
            }
        }
    }
    (rows, None)
}

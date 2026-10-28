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
                EnumValue::Known(V::Unspecified)|EnumValue::Unknown(_)=>"Valeur non prise en charge" }
        }
    }
}
label!(mode,wire::RunMode,Single=>"Single",Restart=>"Restart",Queued=>"Queued",Parallel=>"Parallel");
label!(status,wire::RunStatus,Queued=>"En attente",Running=>"En cours",Completed=>"Terminé",Canceled=>"Annulé",Interrupted=>"Interrompu",Failed=>"Refusé ou échoué");
label!(reason,wire::RunReason,ExplicitRequest=>"Demande explicite",SingleBusy=>"Single déjà occupé",RestartReplaced=>"Remplacé par un nouveau run",WaitingForSlot=>"En attente du précédent",ExplicitCancellation=>"Étapes futures annulées",ServerRestart=>"Redémarrage serveur",SyntheticFailure=>"Erreur synthétique");
label!(compatibility,wire::Compatibility,Supported=>"Prise en charge",Partial=>"Partielle",Unsupported=>"Non prise en charge");
label!(diagnostic,wire::Diagnostic,InvalidSchema=>"Schéma invalide",ExcludedReference=>"Référence exclue",UnsupportedExtension=>"Extension non prise en charge",SourceUnavailable=>"Source indisponible");
label!(gap,wire::GapReason,Evicted=>"Événements évincés",Redacted=>"Événements expurgés",NotRetained=>"Événements non conservés");
label!(action,obs::TraceActionKind,Variables=>"Variables",Sequence=>"Séquence",Choose=>"Choix",If=>"Condition",Repeat=>"Répétition",Parallel=>"Parallèle",Wait=>"Attente",Stop=>"Arrêt",Call=>"Appel",Delay=>"Délai",WaitTemplate=>"Attente de template");
label!(stage,obs::TraceNodeStage,Entered=>"Entrée",Completed=>"Terminé",Stopped=>"Arrêté",Cancelled=>"Annulé",Failed=>"Échec",Waiting=>"En attente");
label!(outcome,history::CommandOutcome,Accepted=>"Admise",Dispatched=>"Dispatchée",Acknowledged=>"Acquittée",Confirmed=>"Confirmée",Indeterminate=>"Indéterminée",Refused=>"Refusée",NotAttempted=>"Non tentée");
fn family(value: EnumValue<plan::SourceFamily>) -> &'static str {
    match value {
        EnumValue::Known(plan::SourceFamily::Automation) => "Automatisation",
        EnumValue::Known(plan::SourceFamily::Script) => "Script",
        EnumValue::Known(plan::SourceFamily::Scene) => "Scène",
        EnumValue::Known(
            plan::SourceFamily::Operational
            | plan::SourceFamily::DeclarativeV1
            | plan::SourceFamily::DeclarativeV2
            | plan::SourceFamily::Authorization
            | plan::SourceFamily::Ownership
            | plan::SourceFamily::Dashboard
            | plan::SourceFamily::Unspecified,
        )
        | EnumValue::Unknown(_) => "Famille non prise en charge",
    }
}
fn actor(run: &wire::Run) -> String {
    match &run.actor.observation {
        Some(wire::actor::Observation::Account(value)) => {
            format!("Acteur enregistré : {}", value.value)
        }
        Some(wire::actor::Observation::Redacted(_)) => "Acteur expurgé par le serveur".into(),
        Some(wire::actor::Observation::NotRecorded(_)) => "Acteur non enregistré".into(),
        None => "Acteur indisponible".into(),
    }
}
fn run_detail(run: &wire::Run) -> String {
    format!(
        "{} · {} · {}\nGénération {} · {} · {}\n{}\nTentative {} · Aucun effet matériel validé",
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
    push(render::fact("logic/mock"),"Logique · serveur synthétique".into(),"Mock NHA-163 explicite. Aucune routine réelle exécutée. Ouvrir cette vue n’active aucune définition.".into());
    push(
        Target::Logic(Selection::Catalogue),
        "Catalogue".into(),
        "Recharger les définitions autorisées depuis le serveur".into(),
    );
    match payload {
        Payload::Catalogue(value) => {
            push(
                render::fact("logic/count"),
                format!(
                    "{} définitions retenues · {} reportées",
                    value.retained, value.reported
                ),
                format!(
                    "Catalogue {} · génération {} · epoch {}",
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
                    "{} · {} · version {}\nHash {}\nSchéma {} · {}",
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
                "Source protégée".into(),
                format!(
                    "Document {}\nRéférence {}\nCes métadonnées ne sont pas une source éditable. La simulation utilise une source fournie séparément dans Configuration.",
                    definition.source_document.value,
                    render::hex(&definition.protected_source_reference)
                ),
            );
            for (index, value) in definition.diagnostics.iter().enumerate() {
                push(
                    render::fact(format!("logic/diagnostic/{index}")),
                    "Diagnostic serveur".into(),
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
                    "Aucun run retenu".into(),
                    "Cette lecture ne lance aucune exécution.".into(),
                );
            }
        }
        Payload::Run { context, run } => {
            push(render::fact("logic/run"), run.id.clone(), run_detail(run));
            push(
                render::fact("logic/run-definition"),
                format!(
                    "Définition {} · version {}",
                    run.definition.id, run.definition.version.revision
                ),
                render::hex(&run.definition.version.source_sha256),
            );
            push(
                Target::Logic(Selection::Trace {
                    context: context.clone(),
                    id: RunId::parse(&run.id).expect("admitted run"),
                }),
                "Inspecter la trace".into(),
                "Capture paginée avec trous et expurgation explicites".into(),
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
                "Revenir au run".into(),
                run_detail(run),
            );
            push(
                render::fact("logic/capture"),
                format!("{} événements · capture {}", events.len(), capture),
                "Capture serveur figée ; aucun ordre continu déduit à travers les trous.".into(),
            );
            for row in events {
                let (title, detail) = match &row.event {
                    Some(wire::trace_row::Event::Node(value)) => {
                        let source = match &value.source {
                            Some(wire::node::Source::Position(value)) => format!(
                                "{}:{}:{} · {}",
                                value.file, value.line, value.column, value.path
                            ),
                            Some(wire::node::Source::Redacted(_)) => "Source expurgée".into(),
                            Some(wire::node::Source::NotRecorded(_)) => {
                                "Source non enregistrée".into()
                            }
                            None => "Source indisponible".into(),
                        };
                        (
                            format!("{} · {}", action(value.kind), stage(value.stage)),
                            source,
                        )
                    }
                    Some(wire::trace_row::Event::Command(value)) => (
                        format!("Commande synthétique · {}", outcome(value.outcome)),
                        format!(
                            "{} · Aucun argument ni appareil réel exposé",
                            value.command_id
                        ),
                    ),
                    Some(wire::trace_row::Event::Gap(value)) => (
                        gap(value.reason).into(),
                        if value.count_unknown {
                            "Quantité inconnue · continuité non établie".into()
                        } else {
                            format!("{} événements manquants", value.known_missing_count)
                        },
                    ),
                    Some(wire::trace_row::Event::Lifecycle(value)) => {
                        ("Cycle de vie".into(), reason(*value).into())
                    }
                    None => ("Événement indisponible".into(), String::new()),
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

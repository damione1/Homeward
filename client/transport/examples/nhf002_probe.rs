//! Current API qualification against the existing isolated Go/SQL TLS fixture.
//! No command is submitted. Three native publication barriers exercise capture
//! and stream ownership without a load benchmark or legacy protocol adapter.
#[path = "support/scenario.rs"]
mod scenario;
use home_client_transport::{AccessToken, Connection, FailureKind};
use home_wire_contract::home::api::v2 as api;
use scenario::Scenario;
use std::{error::Error, io::Write, time::Duration};
use tokio_util::sync::CancellationToken;

fn control(value: &str) {
    println!("{value}");
    std::io::stdout().flush().unwrap();
}
fn power(entry: &api::EntityViewEntry) -> Option<api::PowerValue> {
    entry
        .capabilities
        .iter()
        .find_map(|capability| match capability.r#trait.as_ref()? {
            api::capability::Trait::Power(value) => {
                value.observation.value.and_then(|v| v.as_known())
            }
            _ => None,
        })
}
#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let mut args = std::env::args().skip(1);
    let path = args.next().ok_or("private synthetic fixture required")?;
    let scenario = args
        .next()
        .map(|v| v.parse())
        .transpose()?
        .unwrap_or(Scenario::Full);
    if args.next().is_some() {
        return Err("unexpected argument".into());
    }
    let fixture: serde_json::Value = serde_json::from_slice(&std::fs::read(path)?)?;
    if fixture["mode"] != "synthetic-only" {
        return Err("synthetic fixture required".into());
    }
    let text = |key: &str| fixture[key].as_str().ok_or("missing fixture field");
    let connection = Connection::new(text("origin")?, text("ca_pem")?.as_bytes())?;
    let client = connection.entity_views();
    let token = AccessToken::parse(text("access_token")?)?;
    let cancel = CancellationToken::new();
    let timeout = Duration::from_secs(5);
    let request = api::CreateEntityViewRequest {
        parent: text("home")?.into(),
        entity_view: api::EntityView::default().into(),
        ..Default::default()
    };
    if matches!(scenario, Scenario::WrongName) {
        let failure = client
            .create_entity_view(request, &token, timeout, &cancel)
            .await
            .expect_err("wrong TLS name accepted");
        if failure.kind() != FailureKind::WrongServerName {
            return Err(failure.into());
        }
        control(&format!(
            "NHF002_RESULT {}",
            serde_json::json!({"os":std::env::consts::OS,"wrong_name_rejected":true})
        ));
        return Ok(());
    }
    let untrusted = Connection::new(text("origin")?, text("untrusted_ca_pem")?.as_bytes())?;
    let failure = untrusted
        .entity_views()
        .create_entity_view(request.clone(), &token, timeout, &cancel)
        .await
        .expect_err("untrusted CA accepted");
    if !matches!(
        failure.kind(),
        FailureKind::UntrustedCertificate | FailureKind::TlsRejected
    ) {
        return Err(failure.into());
    }
    let capture = client
        .create_entity_view(request.clone(), &token, timeout, &cancel)
        .await?;
    let page = client
        .query_entity_view(
            api::QueryEntityViewRequest {
                name: capture.name.clone(),
                page_size: 16,
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await?;
    if page.entries.len() != 1
        || page.entries[0].entity.name != text("entity_name")?
        || !page.next_page_token.is_empty()
        || page.resume_token.is_empty()
        || power(&page.entries[0]) != Some(api::PowerValue::Off)
    {
        return Err("initial exact v2 capture missing OFF".into());
    }
    control("NHF002_ADVANCE 1");
    let watch_cancel = CancellationToken::new();
    let mut stream = client
        .watch_entity_view(
            api::WatchEntityViewRequest {
                name: capture.name.clone(),
                resume_token: page.resume_token.clone(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(15),
            &watch_cancel,
        )
        .await?;
    let mut checkpoint = page.resume_token;
    for sequence in 1..=3 {
        if sequence > 1 {
            control(&format!("NHF002_ADVANCE {sequence}"))
        }
        let frame = tokio::time::timeout(timeout, stream.next())
            .await??
            .ok_or("missing native change")?;
        let Some(api::watch_entity_view_response::Change::Upsert(entry)) = frame.change else {
            return Err("expected exact upsert".into());
        };
        let expected = if sequence % 2 == 1 {
            api::PowerValue::On
        } else {
            api::PowerValue::Off
        };
        if entry.entity.name != text("entity_name")?
            || power(&entry) != Some(expected)
            || frame.resume_token.is_empty()
            || frame.resume_token == checkpoint
        {
            return Err("incorrect current stream frame".into());
        }
        checkpoint = frame.resume_token;
    }
    watch_cancel.cancel();
    if stream
        .next()
        .await
        .expect_err("cancelled stream survived")
        .kind()
        != FailureKind::Cancelled
    {
        return Err("cancellation changed meaning".into());
    }
    client
        .get_entity_view(
            api::GetEntityViewRequest {
                name: capture.name.clone(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await?;
    client
        .delete_entity_view(
            api::DeleteEntityViewRequest {
                name: capture.name.clone(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await?;
    let next = client
        .create_entity_view(request, &token, timeout, &cancel)
        .await?;
    let stale = client
        .watch_entity_view(
            api::WatchEntityViewRequest {
                name: next.name.clone(),
                resume_token: checkpoint,
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await;
    let rejected = match stale {
        Err(error) => error,
        Ok(mut stream) => stream
            .next()
            .await
            .expect_err("foreign capture checkpoint accepted"),
    };
    if rejected
        .api_failure()
        .map_err(|_| "invalid canonical capture refusal")?
        .is_none()
    {
        return Err("capture refusal lacks canonical detail".into());
    }
    client
        .delete_entity_view(
            api::DeleteEntityViewRequest {
                name: next.name.clone(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await?;
    control("NHF002_REVOKE");
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        let result = connection
            .home()
            .list_homes(
                api::ListHomesRequest {
                    page_size: 1,
                    ..Default::default()
                },
                &token,
                timeout,
                &cancel,
            )
            .await;
        if let Err(error) = result {
            if error
                .api_failure()
                .map_err(|_| "invalid canonical refusal")?
                .is_none_or(|f| f.reason() != api::ApiErrorReason::AuthenticationRequired)
                || error.may_resume_read()
            {
                return Err("revocation not canonical/fail closed".into());
            }
            break;
        }
        if tokio::time::Instant::now() >= deadline {
            return Err("credential revocation not observed".into());
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    control(&format!(
        "NHF002_RESULT {}",
        serde_json::json!({"os":std::env::consts::OS,"unary":true,"present_false":true,"stream_frames":3,"cancelled":true,"session_still_usable":true,"untrusted_ca_rejected":true,"typed_revocation":true,"stale_capture_rejected":true})
    ));
    Ok(())
}

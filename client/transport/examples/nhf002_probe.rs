//! Qualification executable. Its input is a private synthetic harness fixture,
//! never the application's session store. The host controls fake observations
//! through stdout barriers; all StateService traffic uses the real TLS RPCs.
use std::{
    error::Error,
    future::{Future, poll_fn},
    io::Write,
    task::Poll,
    time::Duration,
};

use buffa::Message;
use home_client_transport::{AccessToken, FailureKind, ReconnectPolicy, StateTransport};
use home_wire_contract::home::auth::v1::AuthorizationTerminationReason;
use home_wire_contract::home::state::v1::{self as state, observed_state::Value};
use tokio_util::sync::CancellationToken;

fn barrier(sequence: u64) {
    println!("NHF002_ADVANCE {sequence}");
    std::io::stdout().flush().expect("flush fixture barrier");
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let path = std::env::args()
        .nth(1)
        .ok_or("private fixture path required")?;
    let fixture: serde_json::Value = serde_json::from_slice(&std::fs::read(path)?)?;
    if fixture["mode"] != "synthetic-only" {
        return Err("only explicit synthetic harness fixtures accepted".into());
    }
    let text = |name: &str| fixture[name].as_str().ok_or("incomplete private fixture");
    let client = StateTransport::new(text("origin")?, text("ca_pem")?.as_bytes())?;
    let token = AccessToken::parse(text("access_token")?)?;
    let wrong_name = std::env::args().nth(2).as_deref() == Some("wrong-name");
    let cancel = CancellationToken::new();
    if wrong_name {
        let failure = client
            .open_view(
                state::OpenViewRequest {
                    filter: state::ViewFilter::default().into(),
                    page_size: 16,
                    ..Default::default()
                },
                &token,
                Duration::from_secs(5),
                &cancel,
            )
            .await
            .expect_err("wrong server name accepted");
        if failure.kind() != FailureKind::WrongServerName {
            return Err(failure.into());
        }
        println!(
            "NHF002_RESULT {}",
            serde_json::json!({"os": std::env::consts::OS, "wrong_name_rejected": true})
        );
        return Ok(());
    }
    let untrusted = StateTransport::new(text("origin")?, text("untrusted_ca_pem")?.as_bytes())?;
    let failure = untrusted
        .open_view(
            state::OpenViewRequest {
                filter: state::ViewFilter::default().into(),
                page_size: 16,
                ..Default::default()
            },
            &token,
            Duration::from_secs(5),
            &cancel,
        )
        .await
        .expect_err("untrusted certificate accepted");
    if failure.kind() != FailureKind::UntrustedCertificate {
        return Err(failure.into());
    }
    let page = client
        .open_view(
            state::OpenViewRequest {
                filter: state::ViewFilter::default().into(),
                page_size: 16,
                ..Default::default()
            },
            &token,
            Duration::from_secs(5),
            &cancel,
        )
        .await?;
    if page.entities.len() != 1 || page.entities[0].state.value != Some(Value::SwitchOn(false)) {
        return Err("snapshot did not preserve the present false value".into());
    }
    barrier(1);
    let mut watch = client
        .watch_view(
            state::WatchViewRequest {
                cursor: page.cursor.clone(),
                filter: state::ViewFilter::default().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(60),
            &cancel,
        )
        .await?;
    for sequence in 1..=1000_u64 {
        let frame = watch
            .next()
            .await?
            .ok_or("stream ended before all frames")?;
        if frame.stream_sequence != sequence {
            return Err("noncontiguous stream frame sequence".into());
        }
        let Some(state::watch_view_response::Frame::Delta(delta)) = frame.frame else {
            return Err("unexpected resync during the ordered frame proof".into());
        };
        if delta.states.len() != 1
            || delta.states[0].value != Some(Value::SwitchOn(sequence % 2 != 0))
        {
            return Err("decoded frame did not preserve the synthetic observation".into());
        }
        if sequence < 1000 {
            barrier(sequence + 1);
        }
    }
    // Poll a real network read to Pending before cancelling it. The future is
    // owned here, with no detached reader task to outlive this scope.
    let mut pending_read = Box::pin(watch.next());
    poll_fn(|cx| match pending_read.as_mut().poll(cx) {
        Poll::Pending => Poll::Ready(Ok(())),
        Poll::Ready(_) => Poll::Ready(Err("read was not pending before cancellation")),
    })
    .await?;
    cancel.cancel();
    let failure = tokio::time::timeout(Duration::from_secs(2), pending_read)
        .await?
        .expect_err("cancelled stream must fail distinctly");
    if failure.kind() != FailureKind::Cancelled {
        return Err("wrong cancellation category".into());
    }
    let alive = client
        .open_view(
            state::OpenViewRequest {
                filter: state::ViewFilter::default().into(),
                page_size: 16,
                ..Default::default()
            },
            &token,
            Duration::from_secs(5),
            &CancellationToken::new(),
        )
        .await?;
    if alive.entities.len() != 1 {
        return Err("cancellation invalidated the session".into());
    }
    let uncancelled = CancellationToken::new();
    let request = state::WatchViewRequest {
        cursor: alive.cursor.clone(),
        filter: state::ViewFilter::default().into(),
        ..Default::default()
    };
    let deadline = match client
        .watch_view(
            request.clone(),
            &token,
            Duration::from_millis(150),
            &uncancelled,
        )
        .await
    {
        Ok(mut stream) => stream
            .next()
            .await
            .expect_err("idle stream missed deadline"),
        Err(failure) => failure,
    };
    if deadline.kind() != FailureKind::Deadline {
        return Err(deadline.into());
    }
    // A different, syntactically valid epoch produces a typed resync followed
    // by a successful EOF, rather than an authentication failure.
    let mut stale = request.clone();
    let epoch = &mut stale.cursor.get_or_insert_default().epoch;
    if epoch.is_empty() {
        return Err("missing epoch".into());
    }
    epoch.replace_range(..1, if epoch.starts_with('0') { "1" } else { "0" });
    let mut ending = client
        .watch_view(stale, &token, Duration::from_secs(5), &uncancelled)
        .await?;
    let frame = ending.next().await?.ok_or("resync missing")?;
    let Some(state::watch_view_response::Frame::ResyncRequired(reason)) = frame.frame else {
        return Err("resync variant lost".into());
    };
    if reason.reason != state::ResyncReason::EpochChanged || ending.next().await?.is_some() {
        return Err("normal EOF was not preserved".into());
    }
    client
        .open_view(
            state::OpenViewRequest {
                filter: state::ViewFilter::default().into(),
                page_size: 16,
                ..Default::default()
            },
            &token,
            Duration::from_secs(5),
            &uncancelled,
        )
        .await?;

    // Cut only this fixture's network while the actual core remains alive.
    barrier(1001);
    let mut interrupted = client
        .watch_view(request, &token, Duration::from_secs(10), &uncancelled)
        .await?;
    let seed = interrupted
        .next()
        .await?
        .ok_or("interruption stream not established")?;
    if seed.stream_sequence != 1
        || !matches!(
            seed.frame,
            Some(state::watch_view_response::Frame::Delta(_))
        )
    {
        return Err("interruption seed missing".into());
    }
    println!("NHF002_OUTAGE");
    std::io::stdout().flush()?;
    let interrupted_error = interrupted
        .next()
        .await
        .expect_err("network cut not observed");
    // connectrpc 0.9.1 reports an interrupted response body as Internal. Keep
    // that code: do not parse its text or retry every server Internal error.
    if interrupted_error.kind() != FailureKind::Rpc(connectrpc::ErrorCode::Internal) {
        return Err(interrupted_error.into());
    }
    let resume = state::WatchViewRequest {
        cursor: seed.cursor,
        filter: state::ViewFilter::default().into(),
        ..Default::default()
    };
    let policy = ReconnectPolicy::new(3, Duration::from_millis(20), Duration::from_millis(40))?;
    let failed = client
        .reconnect_watch(
            resume.clone(),
            &token,
            Duration::from_millis(300),
            &uncancelled,
            policy,
        )
        .await
        .err()
        .ok_or("outage reconnect unexpectedly succeeded")?;
    if failed.kind() != FailureKind::Rpc(connectrpc::ErrorCode::Unavailable) {
        return Err(failed.into());
    }
    let waiting_cancel = CancellationToken::new();
    let waiting_policy = ReconnectPolicy::new(3, Duration::from_secs(10), Duration::from_secs(10))?;
    let mut waiting = Box::pin(waiting_policy.wait_after(1, &waiting_cancel));
    poll_fn(|cx| match waiting.as_mut().poll(cx) {
        Poll::Pending => Poll::Ready(Ok(())),
        Poll::Ready(_) => Poll::Ready(Err("backoff did not wait")),
    })
    .await?;
    waiting_cancel.cancel();
    if waiting
        .await
        .expect_err("backoff ignored cancellation")
        .kind()
        != FailureKind::Cancelled
    {
        return Err("backoff cancellation misclassified".into());
    }
    println!("NHF002_RESTORE");
    std::io::stdout().flush()?;
    barrier(1002);
    let mut resumed = client
        .reconnect_watch(resume, &token, Duration::from_secs(3), &uncancelled, policy)
        .await?;
    let frame = resumed.next().await?.ok_or("reconnected stream missing")?;
    if frame.stream_sequence != 1 {
        return Err("reconnect stream sequence was fabricated".into());
    }
    let Some(state::watch_view_response::Frame::Delta(delta)) = frame.frame else {
        return Err("reconnect delta missing".into());
    };
    if delta.states.len() != 1 || delta.states[0].value != Some(Value::SwitchOn(false)) {
        return Err("reconnect changed observations".into());
    }
    drop(resumed);
    let fresh = client
        .open_view(
            state::OpenViewRequest {
                filter: state::ViewFilter::default().into(),
                page_size: 16,
                ..Default::default()
            },
            &token,
            Duration::from_secs(5),
            &uncancelled,
        )
        .await?;

    let future = StateTransport::new(text("wire_origin")?, text("wire_ca_pem")?.as_bytes())?;
    let future_token = AccessToken::parse("nhf002-future-wire-only")?;
    let unary_deadline = future
        .read_page(
            state::ReadPageRequest {
                snapshot_token: "future-wire-deadline".into(),
                ..Default::default()
            },
            &future_token,
            Duration::from_millis(150),
            &uncancelled,
        )
        .await
        .expect_err("unary deadline was ignored");
    if unary_deadline.kind() != FailureKind::Deadline {
        return Err(unary_deadline.into());
    }
    let wire_page = future
        .read_page(
            state::ReadPageRequest {
                snapshot_token: "future-wire-only".into(),
                ..Default::default()
            },
            &future_token,
            Duration::from_secs(5),
            &uncancelled,
        )
        .await?;
    check_future_values(&wire_page.page.states)?;
    let mut wire_watch = future
        .watch_view(
            state::WatchViewRequest::default(),
            &future_token,
            Duration::from_secs(5),
            &uncancelled,
        )
        .await?;
    let first = wire_watch
        .next()
        .await?
        .ok_or("future fixture missing delta")?;
    let Some(state::watch_view_response::Frame::Delta(values)) = first.frame else {
        return Err("future fixture delta lost".into());
    };
    check_future_values(&values.states)?;
    let unknown = wire_watch.next().await?.ok_or("future frame lost")?;
    if unknown.stream_sequence != 2
        || unknown.frame.is_some()
        || unknown.__buffa_unknown_fields.is_empty()
    {
        return Err("future frame reinterpreted".into());
    }
    let roundtrip = state::WatchViewResponse::decode_from_slice(&unknown.encode_to_vec())?;
    if roundtrip != unknown || wire_watch.next().await?.is_some() {
        return Err("future frame roundtrip or EOF lost".into());
    }
    let authority = match future
        .reconnect_watch(
            state::WatchViewRequest {
                cursor: state::ViewCursor {
                    epoch: "authority-unavailable".into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            &future_token,
            Duration::from_secs(3),
            &uncancelled,
            policy,
        )
        .await
    {
        Err(failure) => failure,
        Ok(mut stream) => stream
            .next()
            .await
            .expect_err("authority failure was accepted"),
    };
    if authority.kind() != FailureKind::Rpc(connectrpc::ErrorCode::Unavailable)
        || authority
            .authorization_termination()
            .map_err(|_| "authority detail invalid")?
            .ok_or("authority detail missing")?
            .reason
            != AuthorizationTerminationReason::AuthorityUnavailable
    {
        return Err("authority termination changed meaning".into());
    }

    // Establish a live reader before revoking only the synthetic native session.
    barrier(1003);
    let mut revoked = client
        .watch_view(
            state::WatchViewRequest {
                cursor: fresh.cursor,
                filter: state::ViewFilter::default().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(10),
            &uncancelled,
        )
        .await?;
    revoked
        .next()
        .await?
        .ok_or("revocation stream not established")?;
    println!("NHF002_REVOKE");
    std::io::stdout().flush()?;
    let termination = revoked.next().await.expect_err("revoked stream accepted");
    if termination.kind() != FailureKind::Rpc(connectrpc::ErrorCode::Unauthenticated) {
        return Err(termination.into());
    }
    let detail = termination
        .authorization_termination()
        .map_err(|_| "invalid termination detail")?
        .ok_or("termination detail lost")?;
    if detail.reason != AuthorizationTerminationReason::CredentialRevoked {
        return Err("wrong typed termination reason".into());
    }
    println!(
        "NHF002_RESULT {}",
        serde_json::json!({"os": std::env::consts::OS, "unary": true, "present_false": true, "stream_frames": 1000, "cancelled": true, "session_still_usable": true, "untrusted_ca_rejected": true, "deadline": true, "unary_deadline": true, "normal_eof": true, "typed_revocation": true, "network_cut": true, "bounded_reconnect": true, "backoff_cancelled": true, "zero_absent_future": true, "authority_not_retried": true})
    );
    Ok(())
}

fn check_future_values(states: &[state::ObservedState]) -> Result<(), Box<dyn Error>> {
    if states.len() != 6 || states[0].value != Some(Value::SwitchOn(false)) {
        return Err("false lost".into());
    }
    let Some(Value::Measurement(zero)) = &states[1].value else {
        return Err("zero variant lost".into());
    };
    if zero.value != 0.0 || zero.decimal_value.is_some() || states[1].source_seq != Some(0) {
        return Err("present zero/absent decimal lost".into());
    }
    let Some(Value::Light(light)) = &states[2].value else {
        return Err("light variant lost".into());
    };
    if light.on.is_some()
        || light.brightness_percent.is_some()
        || light.color_temp_mireds.is_some()
        || states[3].value.is_some()
        || states[3].observed_at.as_option().is_some()
    {
        return Err("absence turned into a reading".into());
    }
    if states[4].value.is_some() || states[4].__buffa_unknown_fields.is_empty() {
        return Err("future value turned into a known value".into());
    }
    let roundtrip = state::ObservedState::decode_from_slice(&states[4].encode_to_vec())?;
    if roundtrip != states[4] {
        return Err("future value discarded".into());
    }
    let golden = &states[5];
    let Some(Value::Measurement(measurement)) = &golden.value else {
        return Err("golden measurement lost".into());
    };
    if golden.quality.availability.to_i32() != 99
        || golden.revision != u64::MAX
        || golden.source_seq != Some(0)
        || golden.observed_at.nanos != 123456789
        || measurement.decimal_value.as_deref() != Some("0.123456789012345678")
    {
        return Err("golden future enum/precision changed".into());
    }
    Ok(())
}

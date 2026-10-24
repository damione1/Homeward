use std::time::Duration;

use async_trait::async_trait;
use buffa::EnumValue;
use home_client_session::AccessLease;
use home_client_transport::{Failure, StateTransport, StateWatch};
use home_wire_contract::home::state::v1 as wire;
use tokio_util::sync::CancellationToken;

use crate::{CacheFailure, CachedView, FrameEffect, LiveView};

/// The caller must erase its pending visual projection before handling Resync,
/// Invalid, Cancelled or a Transport exit without a retained view.
pub enum WatchExit {
    Ended(CachedView),
    Transport {
        failure: Failure,
        retained: Option<CachedView>,
    },
    Resync(EnumValue<wire::ResyncReason>),
    Invalid(CacheFailure),
    Cancelled,
}

#[async_trait]
trait FrameReader: Send {
    async fn next(&mut self) -> Result<Option<wire::WatchViewResponse>, Failure>;
}

#[async_trait]
impl FrameReader for StateWatch {
    async fn next(&mut self) -> Result<Option<wire::WatchViewResponse>, Failure> {
        StateWatch::next(self).await
    }
}

fn transport_exit(failure: Failure, retained: CachedView) -> WatchExit {
    // Even Unavailable can mean an explicit authority termination. TLS identity,
    // auth, malformed typed details and unknown RPC failures never retain data.
    let offline = failure.may_resume_read();
    WatchExit::Transport {
        failure,
        retained: offline.then_some(retained),
    }
}

/// One stream attempt from the complete last-applied cursor and full filter.
/// Apply every frame off the UI thread; coalesce only inside on_applied's visual
/// mailbox. No background task or raw frame queue survives this function.
pub async fn watch_once(
    transport: &StateTransport,
    lease: &AccessLease,
    view: CachedView,
    timeout: Duration,
    cancel: &CancellationToken,
    on_applied: impl FnMut(&CachedView) + Send,
) -> WatchExit {
    if cancel.is_cancelled() || lease.cancellation.is_cancelled() {
        return WatchExit::Cancelled;
    }
    let request = wire::WatchViewRequest {
        cursor: view.cursor().clone().into(),
        filter: view.filter().to_wire().into(),
        ..Default::default()
    };
    let stream = tokio::select! {
        biased;
        () = cancel.cancelled() => return WatchExit::Cancelled,
        () = lease.cancellation.cancelled() => return WatchExit::Cancelled,
        result = transport.watch_view(request, lease.token(), timeout, &lease.cancellation) => result,
    };
    if cancel.is_cancelled() || lease.cancellation.is_cancelled() {
        return WatchExit::Cancelled;
    }
    match stream {
        Ok(mut stream) => drive(&mut stream, view, cancel, &lease.cancellation, on_applied).await,
        Err(failure) => transport_exit(failure, view),
    }
}

async fn drive(
    stream: &mut impl FrameReader,
    view: CachedView,
    cancel: &CancellationToken,
    access: &CancellationToken,
    mut on_applied: impl FnMut(&CachedView) + Send,
) -> WatchExit {
    let mut live = LiveView::new(view);
    loop {
        let response = tokio::select! {
            biased;
            () = cancel.cancelled() => return WatchExit::Cancelled,
            () = access.cancelled() => return WatchExit::Cancelled,
            result = stream.next() => result,
        };
        if cancel.is_cancelled() || access.is_cancelled() {
            return WatchExit::Cancelled;
        }
        match response {
            Ok(Some(frame)) => match live.apply(frame) {
                Ok(FrameEffect::Applied) => {
                    on_applied(live.current().expect("applied frame retains its view"))
                }
                Ok(FrameEffect::Resync(reason)) => return WatchExit::Resync(reason),
                Err(failure) => return WatchExit::Invalid(failure),
            },
            Ok(None) => {
                return WatchExit::Ended(
                    live.into_current().expect("active stream retains its view"),
                );
            }
            Err(failure) => {
                return transport_exit(
                    failure,
                    live.into_current().expect("active stream retains its view"),
                );
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{DeliveryTicket, Filter, ProjectionMailbox, Publication};
    use connectrpc::{ConnectError, ErrorDetail};
    use home_wire_contract::home::registry::v1 as registry;
    use std::{
        collections::{BTreeMap, VecDeque},
        sync::Arc,
    };

    struct Reader {
        frames: VecDeque<Result<Option<wire::WatchViewResponse>, Failure>>,
    }
    #[async_trait]
    impl FrameReader for Reader {
        async fn next(&mut self) -> Result<Option<wire::WatchViewResponse>, Failure> {
            self.frames.pop_front().unwrap_or(Ok(None))
        }
    }

    fn view() -> CachedView {
        let filter = Filter::all();
        let cursor = wire::ViewCursor {
            epoch: "fixture-epoch".into(),
            config_revision: 1,
            authorization_revision: 1,
            global_sequence: 100,
            filter_sha256: filter.digest().to_vec(),
            config_sha256: vec![1; 32],
            grant_sha256: vec![2; 32],
            admission_proof: vec![3; 40],
            ..Default::default()
        };
        let entities = (0..850)
            .map(|i| {
                let id = format!("opaque-{i}");
                (
                    id.parse().unwrap(),
                    Arc::new(wire::ViewEntity {
                        entity_id: id,
                        ..Default::default()
                    }),
                )
            })
            .collect::<BTreeMap<_, _>>();
        CachedView {
            cursor,
            filter,
            entities,
            encoded_bytes: 100_000,
        }
    }

    fn frame(sequence: u64) -> wire::WatchViewResponse {
        let mut cursor = view().cursor;
        cursor.global_sequence += sequence * 3;
        wire::WatchViewResponse {
            stream_sequence: sequence,
            cursor: cursor.into(),
            frame: Some(wire::watch_view_response::Frame::Delta(Box::new(
                wire::StateDelta {
                    states: vec![wire::ObservedState {
                        entity: registry::EntityRef {
                            reference: Some(registry::entity_ref::Reference::EntityId(
                                "opaque-0".into(),
                            )),
                            ..Default::default()
                        }
                        .into(),
                        revision: sequence,
                        value: Some(wire::observed_state::Value::SwitchOn(
                            sequence.is_multiple_of(2),
                        )),
                        ..Default::default()
                    }],
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
    }

    #[tokio::test]
    async fn all_thousand_frames_apply_to_850_entities_while_only_one_ui_notification_waits() {
        let mut reader = Reader {
            frames: (1..=1000).map(|i| Ok(Some(frame(i)))).collect(),
        };
        let mailbox = ProjectionMailbox::new();
        let generation = mailbox.generation();
        let mut ticket: Option<DeliveryTicket<u64>> = None;
        let mut applied = 0;
        let exit = drive(
            &mut reader,
            view(),
            &CancellationToken::new(),
            &CancellationToken::new(),
            |view| {
                applied += 1;
                assert_eq!(view.len(), 850);
                assert_eq!(
                    view.get(&"opaque-0".parse().unwrap())
                        .unwrap()
                        .state
                        .revision,
                    applied
                );
                match mailbox.publish(generation, view.cursor().global_sequence) {
                    Publication::Schedule(value) => {
                        assert!(ticket.is_none());
                        ticket = Some(value);
                    }
                    Publication::Coalesced => {}
                    Publication::Rejected(_) => panic!("current projection rejected"),
                }
            },
        )
        .await;
        assert_eq!(applied, 1000);
        match exit {
            WatchExit::Ended(view) => assert_eq!(view.cursor().global_sequence, 3100),
            WatchExit::Transport { .. }
            | WatchExit::Resync(_)
            | WatchExit::Invalid(_)
            | WatchExit::Cancelled => panic!("unexpected exit"),
        }
        assert_eq!(mailbox.metrics().pending_notifications, 1);
        assert_eq!(mailbox.metrics().scheduled, 1);
        assert_eq!(mailbox.metrics().coalesced, 999);
        ticket
            .unwrap()
            .deliver(|_, cursor| assert_eq!(cursor, 3100));
    }

    #[tokio::test]
    async fn a_network_cut_returns_only_the_cursor_that_was_actually_applied() {
        let mut reader = Reader {
            frames: VecDeque::from([
                Ok(Some(frame(1))),
                Err(Failure::from(ConnectError::unavailable("fixture-cut"))),
            ]),
        };
        let exit = drive(
            &mut reader,
            view(),
            &CancellationToken::new(),
            &CancellationToken::new(),
            |_| {},
        )
        .await;
        match exit {
            WatchExit::Transport {
                retained: Some(view),
                ..
            } => assert_eq!(view.cursor().global_sequence, 103),
            WatchExit::Transport { retained: None, .. }
            | WatchExit::Ended(_)
            | WatchExit::Resync(_)
            | WatchExit::Invalid(_)
            | WatchExit::Cancelled => panic!("lost applied resume cursor"),
        }
    }

    #[tokio::test]
    async fn authority_and_permission_failures_never_return_a_stale_private_view() {
        let malformed_authority =
            ConnectError::unavailable("fixture-authority").with_detail(ErrorDetail {
                type_url: "type.googleapis.com/home.auth.v1.AuthorizationTermination".into(),
                value: Some("%%%".into()),
                debug: None,
            });
        for error in [
            malformed_authority,
            ConnectError::permission_denied("fixture"),
            ConnectError::unauthenticated("fixture"),
        ] {
            let mut reader = Reader {
                frames: VecDeque::from([Ok(Some(frame(1))), Err(Failure::from(error))]),
            };
            let exit = drive(
                &mut reader,
                view(),
                &CancellationToken::new(),
                &CancellationToken::new(),
                |_| {},
            )
            .await;
            assert!(matches!(exit, WatchExit::Transport { retained: None, .. }));
        }
    }

    #[tokio::test]
    async fn resync_and_out_of_order_frames_stop_before_any_later_frame_is_applied() {
        let terminal = wire::WatchViewResponse {
            stream_sequence: 2,
            frame: Some(wire::watch_view_response::Frame::ResyncRequired(Box::new(
                wire::ResyncRequired {
                    reason: wire::ResyncReason::CapabilitiesChanged.into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        let mut reader = Reader {
            frames: VecDeque::from([Ok(Some(frame(1))), Ok(Some(terminal)), Ok(Some(frame(3)))]),
        };
        let mut applied = 0;
        let exit = drive(
            &mut reader,
            view(),
            &CancellationToken::new(),
            &CancellationToken::new(),
            |_| applied += 1,
        )
        .await;
        assert_eq!(applied, 1);
        assert!(matches!(exit, WatchExit::Resync(_)));
        assert_eq!(reader.frames.len(), 1);
        let mut reader = Reader {
            frames: VecDeque::from([Ok(Some(frame(2))), Ok(Some(frame(3)))]),
        };
        let exit = drive(
            &mut reader,
            view(),
            &CancellationToken::new(),
            &CancellationToken::new(),
            |_| panic!("invalid frame published"),
        )
        .await;
        assert!(matches!(exit, WatchExit::Invalid(CacheFailure::OutOfOrder)));
    }

    #[tokio::test]
    async fn an_access_cancellation_before_a_read_consumes_no_frame() {
        let mut reader = Reader {
            frames: VecDeque::from([Ok(Some(frame(1)))]),
        };
        let access = CancellationToken::new();
        access.cancel();
        let exit = drive(
            &mut reader,
            view(),
            &CancellationToken::new(),
            &access,
            |_| panic!("cancelled view published"),
        )
        .await;
        assert!(matches!(exit, WatchExit::Cancelled));
        assert_eq!(reader.frames.len(), 1);
    }

    #[tokio::test]
    async fn cancellation_racing_with_a_received_frame_cannot_publish_it() {
        struct CancelOnNext(CancellationToken);
        #[async_trait]
        impl FrameReader for CancelOnNext {
            async fn next(&mut self) -> Result<Option<wire::WatchViewResponse>, Failure> {
                self.0.cancel();
                Ok(Some(frame(1)))
            }
        }
        let access = CancellationToken::new();
        let mut reader = CancelOnNext(access.clone());
        let exit = drive(
            &mut reader,
            view(),
            &CancellationToken::new(),
            &access,
            |_| {
                panic!("a frame from an invalidated generation reached publication");
            },
        )
        .await;
        assert!(matches!(exit, WatchExit::Cancelled));
    }
}

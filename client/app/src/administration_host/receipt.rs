//! A just-returned recovery batch is a result of an authorized mutation, not a
//! retained administrator view. Its credential is retired before presentation.
use crate::HomewardWindow;
use home_client_session::{Generation, Phase, SessionCoordinator};
use std::sync::{Arc, Mutex};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

#[derive(Clone)]
pub(super) struct Receipts(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    window: Option<slint::Weak<HomewardWindow>>,
}
struct State {
    version: u64,
    cancel: CancellationToken,
    record: Option<Record>,
}
struct Record {
    source: SessionCoordinator,
    generation: Generation,
    value: Zeroizing<String>,
}
#[derive(Clone)]
pub(super) struct Permit {
    version: u64,
    pub cancel: CancellationToken,
}
impl Receipts {
    pub fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State {
                version: 1,
                cancel: CancellationToken::new(),
                record: None,
            }),
            window,
        }))
    }
    pub fn bind(&self, window: &HomewardWindow) {
        let owner = self.clone();
        window.on_close_recovery_receipt(move || owner.clear());
        window.set_recovery_receipt("".into());
    }
    pub fn permit(&self) -> Permit {
        let state = self.0.state.lock().expect("receipt state");
        Permit {
            version: state.version,
            cancel: state.cancel.clone(),
        }
    }
    pub fn clear(&self) {
        let version = {
            let mut state = self.0.state.lock().expect("receipt state");
            state.cancel.cancel();
            state.record = None;
            if let Some(next) = state.version.checked_add(1) {
                state.cancel = CancellationToken::new();
                state.version = next;
            }
            state.version
        };
        if let Some(window) = &self.0.window {
            let owner = self.clone();
            let _ = window.upgrade_in_event_loop(move |window| {
                if owner.0.state.lock().expect("receipt state").version == version {
                    window.set_recovery_receipt("".into());
                }
            });
        }
    }
    pub fn revalidate(&self) {
        let stale = self
            .0
            .state
            .lock()
            .expect("receipt state")
            .record
            .as_ref()
            .is_some_and(|record| !record.source.is_current(record.generation));
        if stale {
            self.clear();
        }
    }
    pub fn deliver(
        &self,
        permit: &Permit,
        source: SessionCoordinator,
        generation: Generation,
        value: Zeroizing<String>,
    ) {
        if permit.cancel.is_cancelled()
            || !source.is_current(generation)
            || !matches!(
                source.subscribe().borrow().phase,
                Phase::SignedOut | Phase::StorageUnavailable
            )
        {
            return;
        }
        {
            let mut state = self.0.state.lock().expect("receipt state");
            if state.version != permit.version || permit.cancel.is_cancelled() {
                return;
            }
            state.record = Some(Record {
                source,
                generation,
                value,
            });
        }
        if let Some(window) = &self.0.window {
            let owner = self.clone();
            let version = permit.version;
            let _ = window.upgrade_in_event_loop(move |window| {
                let state = owner.0.state.lock().expect("receipt state");
                if state.version == version
                    && let Some(record) = &state.record
                    && record.source.is_current(record.generation)
                {
                    window.set_recovery_receipt(record.value.as_str().into());
                }
            });
        }
    }
    #[cfg(test)]
    pub fn value(&self) -> Option<Zeroizing<String>> {
        self.0
            .state
            .lock()
            .unwrap()
            .record
            .as_ref()
            .map(|record| record.value.clone())
    }
}

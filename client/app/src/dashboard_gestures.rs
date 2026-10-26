//! Window-owned confirmation retains one exact prepared intention. It never
//! submits by row index again and cannot replay a consumed or stale prompt.
use crate::{
    DialogState, HomewardWindow, ProRecipeGesture,
    device_view::{NativePrepared, Refusal},
};
use home_client_model::dashboard::Gesture;
use slint::ComponentHandle;
use std::{cell::RefCell, rc::Rc, time::Duration};

#[derive(Clone, Copy, PartialEq, Eq)]
struct ConfirmationId(i32);
struct Pending {
    id: ConfirmationId,
    prepared: NativePrepared,
}
#[derive(Default)]
struct State {
    serial: i32,
    pending: Option<Pending>,
}
impl State {
    fn take(&mut self, id: ConfirmationId) -> Option<NativePrepared> {
        if self
            .pending
            .as_ref()
            .is_some_and(|pending| pending.id == id)
        {
            self.pending.take().map(|pending| pending.prepared)
        } else {
            None
        }
    }
}
fn close(window: &HomewardWindow, timer: &slint::Timer) {
    timer.stop();
    window.set_recipe_confirmation(DialogState::default());
    window.set_recipe_confirmation_title("".into());
    window.set_recipe_confirmation_message("".into());
    window.set_recipe_confirmation_description("".into());
}

pub(crate) fn bind(
    window: &HomewardWindow,
    prepare: impl Fn(i32, i32, Gesture) -> Result<NativePrepared, Refusal> + 'static,
    dispatch: impl Fn(NativePrepared) + 'static,
    restore: impl Fn(&NativePrepared) + 'static,
) {
    let state = Rc::new(RefCell::new(State::default()));
    let dispatch = Rc::new(dispatch);
    let restore = Rc::new(restore);
    let timer = Rc::new(slint::Timer::default());
    let (shared, weak, expiry) = (state.clone(), window.as_weak(), Rc::downgrade(&timer));
    let refresh = Rc::new(move || {
        let Some(window) = weak.upgrade() else {
            return;
        };
        let invalid = shared
            .borrow()
            .pending
            .as_ref()
            .is_some_and(|pending| !window.get_view_loaded() || !pending.prepared.is_current());
        if invalid {
            shared.borrow_mut().pending = None;
            if let Some(timer) = expiry.upgrade() {
                close(&window, &timer);
            }
            if window.get_view_loaded() {
                window.set_action_feedback(Refusal::Stale.message().into());
            }
        }
    });
    let (shared, weak, clock, check, execute) = (
        state.clone(),
        window.as_weak(),
        timer.clone(),
        refresh.clone(),
        dispatch.clone(),
    );
    window.on_recipe_gesture(move |token, generation, gesture| {
        let Some(window) = weak.upgrade() else {
            return;
        };
        if shared.borrow().pending.is_some() || window.get_recipe_confirmation().open {
            return;
        }
        let gesture = match gesture {
            ProRecipeGesture::Tap => Gesture::Tap,
            ProRecipeGesture::Hold => Gesture::Hold,
            ProRecipeGesture::DoubleTap => Gesture::DoubleTap,
        };
        match prepare(token, generation, gesture) {
            Ok(prepared) if prepared.is_current() => {
                if let Some(message) = prepared.confirmation.as_ref() {
                    let Some(serial) = shared.borrow().serial.checked_add(1) else {
                        window.set_action_feedback(Refusal::Capacity.message().into());
                        return;
                    };
                    window.set_recipe_confirmation_id(serial);
                    window.set_recipe_confirmation_title(if prepared.title.is_empty() {
                        "Confirmer l’interaction".into()
                    } else {
                        prepared.title.as_str().into()
                    });
                    window.set_recipe_confirmation_message(if message.is_empty() {
                        "Confirmer cette interaction ?".into()
                    } else {
                        message.as_str().into()
                    });
                    window
                        .set_recipe_confirmation_description(prepared.description.as_str().into());
                    *shared.borrow_mut() = State {
                        serial,
                        pending: Some(Pending {
                            id: ConfirmationId(serial),
                            prepared,
                        }),
                    };
                    window.set_recipe_confirmation(DialogState {
                        open: true,
                        topmost: true,
                        busy: false,
                    });
                    let tick = check.clone();
                    clock.start(
                        slint::TimerMode::Repeated,
                        Duration::from_secs(1),
                        move || tick(),
                    );
                } else {
                    execute(prepared);
                }
            }
            Ok(_) => window.set_action_feedback(Refusal::Stale.message().into()),
            Err(refusal) => window.set_action_feedback(refusal.message().into()),
        }
    });
    let (shared, weak, clock, execute) = (state.clone(), window.as_weak(), timer.clone(), dispatch);
    let return_focus = restore.clone();
    window.on_recipe_confirm(move |id| {
        let Some(prepared) = shared.borrow_mut().take(ConfirmationId(id)) else {
            return;
        };
        let Some(window) = weak.upgrade() else {
            return;
        };
        close(&window, &clock);
        if prepared.is_current() {
            return_focus(&prepared);
            execute(prepared);
        } else {
            window.set_action_feedback(Refusal::Stale.message().into());
        }
    });
    let (shared, weak, clock) = (state, window.as_weak(), timer);
    window.on_recipe_dismiss(move |id, _reason| {
        let prepared = shared.borrow_mut().take(ConfirmationId(id));
        if let Some(prepared) = prepared
            && let Some(window) = weak.upgrade()
        {
            close(&window, &clock);
            if prepared.is_current() {
                restore(&prepared);
            }
        }
    });
    window.on_recipe_context_changed(move || refresh());
}

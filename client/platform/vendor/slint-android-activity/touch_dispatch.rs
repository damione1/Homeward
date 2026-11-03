// SPDX-License-Identifier: GPL-3.0-or-later
use crate::touch_guard::{Delivery, Phase, TouchGuard, TouchId};
use i_slint_core::{
    api::Window,
    input::TouchPhase,
    lengths::LogicalPoint,
    platform::{InternalEvent, WindowEvent},
};

// Cancel before Slint 1.18.1 converts Cancelled into Released + Exit. Only a
// single native finger is admitted; a multi-touch sequence stays suppressed
// until every finger lifts, including synthetic re-presses from TouchState.
pub(crate) fn dispatch_touch(
    window: &Window,
    guard: &mut TouchGuard,
    id: i32,
    position: LogicalPoint,
    phase: TouchPhase,
) {
    let Ok(id_value) = u64::try_from(id) else {
        window.dispatch_event(WindowEvent::PointerExited);
        return;
    };
    let native_phase = match phase {
        TouchPhase::Started => Phase::Started,
        TouchPhase::Moved => Phase::Moved,
        TouchPhase::Ended => Phase::Ended,
        TouchPhase::Cancelled => Phase::Cancelled,
    };
    match guard.observe(TouchId(id_value), native_phase) {
        Delivery::Propagate => {}
        Delivery::CancelAndPropagate => window.dispatch_event(WindowEvent::PointerExited),
        Delivery::CancelAndSuppress => {
            window.dispatch_event(WindowEvent::PointerExited);
            return;
        }
        Delivery::Suppress => return,
    }
    window.dispatch_event(WindowEvent::internal(InternalEvent::Touch {
        id,
        position,
        phase,
    }));
}

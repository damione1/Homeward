//! Exercise the exact Android dispatch helper against Slint's real TouchState.
use i_slint_core::{input::TouchPhase, lengths::LogicalPoint};
use slint::ComponentHandle;
#[path = "../../platform/vendor/slint-android-activity/touch_dispatch.rs"]
mod touch_dispatch;
#[path = "../../platform/vendor/slint-android-activity/touch_guard.rs"]
pub(crate) mod touch_guard;
slint::slint! {
    export component TouchFixture inherits Window {
        width: 200px; height: 200px;
        in-out property <bool> active;
        in-out property <int> commits;
        TouchArea {
            pointer-event(event) => {
                if (event.kind == PointerEventKind.down) { root.active = true; }
                if (event.kind == PointerEventKind.up && root.active) { root.active = false; root.commits += 1; }
                if (event.kind == PointerEventKind.cancel) { root.active = false; }
            }
        }
    }
}
#[test]
fn native_cancel_and_both_multi_touch_release_orders_never_commit() {
    i_slint_backend_testing::init_no_event_loop();
    let window = TouchFixture::new().unwrap();
    window.show().unwrap();
    let mut guard = touch_guard::TouchGuard::default();
    let p = LogicalPoint::new(60., 60.);
    for cancelled in [true, false] {
        for primary_first in [true, false] {
            let before = window.get_commits();
            touch_dispatch::dispatch_touch(window.window(), &mut guard, 0, p, TouchPhase::Started);
            assert!(window.get_active());
            if cancelled {
                touch_dispatch::dispatch_touch(
                    window.window(),
                    &mut guard,
                    0,
                    p,
                    TouchPhase::Cancelled,
                );
            } else {
                touch_dispatch::dispatch_touch(
                    window.window(),
                    &mut guard,
                    1,
                    p,
                    TouchPhase::Started,
                );
                for id in if primary_first { [0, 1] } else { [1, 0] } {
                    touch_dispatch::dispatch_touch(
                        window.window(),
                        &mut guard,
                        id,
                        p,
                        TouchPhase::Ended,
                    );
                }
            }
            assert!(!window.get_active());
            assert_eq!(window.get_commits(), before);
            touch_dispatch::dispatch_touch(window.window(), &mut guard, 0, p, TouchPhase::Started);
            touch_dispatch::dispatch_touch(window.window(), &mut guard, 0, p, TouchPhase::Moved);
            touch_dispatch::dispatch_touch(window.window(), &mut guard, 0, p, TouchPhase::Ended);
            assert_eq!(window.get_commits(), before + 1);
        }
    }
}

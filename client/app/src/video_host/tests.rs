use super::*;
#[test]
fn retirement_drops_private_frame_cancels_the_worker_and_advances_identity() {
    let mut state = State::default();
    let cancellation = state.cancel.clone();
    let picture = Arc::new(Picture {
        pixels: slint::SharedPixelBuffer::new(2, 2),
        deadline: Instant::now() + Duration::from_secs(5),
        presentation_micros: 0,
    });
    let weak = Arc::downgrade(&picture);
    state.phase = Playback::Playing(picture);
    let generation = state.generation;
    state.retire(Playback::Paused(PauseReason::User));
    assert!(cancellation.is_cancelled());
    assert!(weak.upgrade().is_none());
    assert!(state.generation > generation);
    assert!(state.phase.can_play());
    assert!(!state.phase.can_pause());
}
#[test]
fn overflow_and_unbound_sources_cannot_start_a_video_job() {
    let mut state = State::default();
    assert!(!state.current());
    state.context = u64::MAX;
    state.retire(Playback::Connecting);
    assert!(!state.current());
    assert!(state.selection.is_none());
    let mut state = State {
        generation: i32::MAX,
        ..Default::default()
    };
    state.retire(Playback::Connecting);
    assert!(!state.current());
    assert!(!state.requested);
}

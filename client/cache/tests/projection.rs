use home_client_cache::{
    DeliveryOutcome, DeliveryTicket, ProjectionMailbox, Publication, PublicationRefusal,
};
use std::sync::{Arc, mpsc};

fn scheduled<T>(result: Publication<T>) -> DeliveryTicket<T> {
    match result {
        Publication::Schedule(ticket) => ticket,
        Publication::Coalesced | Publication::Rejected(_) => panic!("expected a new notification"),
    }
}

#[test]
fn a_slow_ui_holds_one_notification_and_receives_only_the_latest_projection() {
    let mailbox = ProjectionMailbox::new();
    let generation = mailbox.generation();
    let ticket = scheduled(mailbox.publish(generation, 0));
    for i in 1..10_000 {
        assert!(matches!(
            mailbox.publish(generation, i),
            Publication::Coalesced
        ));
        assert_eq!(mailbox.metrics().pending_notifications, 1);
    }
    let mut displayed = None;
    assert_eq!(
        ticket.deliver(|actual_generation, value| {
            assert_eq!(actual_generation, generation);
            displayed = Some(value);
        }),
        DeliveryOutcome::Delivered
    );
    assert_eq!(displayed, Some(9_999));
    assert_eq!(mailbox.metrics().scheduled, 1);
    assert_eq!(mailbox.metrics().coalesced, 9_999);
    assert_eq!(mailbox.metrics().delivered, 1);
    assert_eq!(mailbox.metrics().pending_notifications, 0);
    drop(scheduled(mailbox.publish(generation, 10_000)));
    assert_eq!(mailbox.metrics().pending_notifications, 0);
}

#[test]
fn invalidation_cancels_old_producers_without_queuing_a_second_callback() {
    let mailbox = ProjectionMailbox::new();
    let old = mailbox.generation();
    let ticket = scheduled(mailbox.publish(old, "private-old-view"));
    let new = mailbox.invalidate().unwrap();
    assert_ne!(old, new);
    assert!(matches!(
        mailbox.publish(old, "late-old-work"),
        Publication::Rejected(PublicationRefusal::Obsolete)
    ));
    assert!(matches!(
        mailbox.publish(new, "replacement-view"),
        Publication::Coalesced
    ));
    assert_eq!(mailbox.metrics().pending_notifications, 1);
    assert_eq!(
        ticket.deliver(|generation, value| {
            assert_eq!(generation, new);
            assert_eq!(value, "replacement-view");
        }),
        DeliveryOutcome::Delivered
    );
    let ticket = scheduled(mailbox.publish(new, "protected-view"));
    mailbox.invalidate().unwrap();
    assert_eq!(
        ticket.deliver(|_, _| panic!("invalidated data reached the renderer")),
        DeliveryOutcome::Empty
    );
}

#[test]
fn close_or_loss_of_the_owner_prevents_queued_private_data_from_reappearing() {
    let mailbox = ProjectionMailbox::new();
    let generation = mailbox.generation();
    let ticket = scheduled(mailbox.publish(generation, "protected-view"));
    mailbox.close();
    assert!(matches!(
        mailbox.publish(generation, "late-work"),
        Publication::Rejected(PublicationRefusal::Closed)
    ));
    assert_eq!(
        ticket.deliver(|_, _| panic!("closed renderer called")),
        DeliveryOutcome::Closed
    );
    let mailbox = ProjectionMailbox::new();
    let ticket = scheduled(mailbox.publish(mailbox.generation(), "protected-view"));
    drop(mailbox);
    assert_eq!(
        ticket.deliver(|_, _| panic!("orphan renderer called")),
        DeliveryOutcome::OwnerGone
    );
}

#[test]
fn invalidation_joins_an_in_progress_renderer_before_acknowledging_the_new_generation() {
    let mailbox = Arc::new(ProjectionMailbox::new());
    let old = mailbox.generation();
    let ticket = scheduled(mailbox.publish(old, 1));
    let (entered_tx, entered_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let renderer = std::thread::spawn(move || {
        ticket.deliver(|_, _| {
            entered_tx.send(()).unwrap();
            release_rx.recv().unwrap();
        })
    });
    entered_rx.recv().unwrap();
    let invalidate_mailbox = mailbox.clone();
    let (attempt_tx, attempt_rx) = mpsc::channel();
    let (done_tx, done_rx) = mpsc::channel();
    let invalidator = std::thread::spawn(move || {
        attempt_tx.send(()).unwrap();
        done_tx.send(invalidate_mailbox.invalidate()).unwrap();
    });
    attempt_rx.recv().unwrap();
    assert_eq!(done_rx.try_recv(), Err(mpsc::TryRecvError::Empty));
    release_tx.send(()).unwrap();
    assert_eq!(renderer.join().unwrap(), DeliveryOutcome::Delivered);
    let new = done_rx.recv().unwrap().unwrap();
    invalidator.join().unwrap();
    assert_ne!(new, old);
    assert!(matches!(
        mailbox.publish(old, 2),
        Publication::Rejected(PublicationRefusal::Obsolete)
    ));
}

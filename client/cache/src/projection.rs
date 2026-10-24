//! A single replaceable visual projection, independent of reliable event delivery.
use std::sync::{Arc, Mutex, MutexGuard, Weak};

/// Independent of access-token rotation and transport stream sequence.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ProjectionGeneration(u64);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PublicationRefusal {
    Closed,
    Obsolete,
}

pub enum Publication<T> {
    Schedule(DeliveryTicket<T>),
    Coalesced,
    Rejected(PublicationRefusal),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DeliveryOutcome {
    Delivered,
    Empty,
    OwnerGone,
    Closed,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct ProjectionMetrics {
    pub scheduled: u64,
    pub coalesced: u64,
    pub delivered: u64,
    pub pending_notifications: usize,
}

struct Slot<T> {
    generation: ProjectionGeneration,
    latest: Option<T>,
    pending: bool,
    closed: bool,
    metrics: ProjectionMetrics,
}

fn lock<T>(inner: &Mutex<Slot<T>>) -> MutexGuard<'_, Slot<T>> {
    match inner.lock() {
        Ok(guard) => guard,
        Err(poisoned) => {
            // A panicking renderer must not leave a usable protected projection.
            let mut guard = poisoned.into_inner();
            guard.closed = true;
            guard.latest = None;
            guard
        }
    }
}

/// Only visual snapshots may be coalesced here. Errors and command confirmations
/// belong to separate reliable paths; this mailbox never takes ownership of them.
pub struct ProjectionMailbox<T> {
    inner: Arc<Mutex<Slot<T>>>,
}

impl<T> Default for ProjectionMailbox<T> {
    fn default() -> Self {
        Self::new()
    }
}

impl<T> ProjectionMailbox<T> {
    pub fn new() -> Self {
        Self {
            inner: Arc::new(Mutex::new(Slot {
                generation: ProjectionGeneration(1),
                latest: None,
                pending: false,
                closed: false,
                metrics: ProjectionMetrics::default(),
            })),
        }
    }

    pub fn generation(&self) -> ProjectionGeneration {
        lock(&self.inner).generation
    }

    pub fn publish(&self, generation: ProjectionGeneration, value: T) -> Publication<T> {
        let mut slot = lock(&self.inner);
        if slot.closed {
            return Publication::Rejected(PublicationRefusal::Closed);
        }
        if generation != slot.generation {
            return Publication::Rejected(PublicationRefusal::Obsolete);
        }
        slot.latest = Some(value);
        if slot.pending {
            slot.metrics.coalesced = slot.metrics.coalesced.saturating_add(1);
            Publication::Coalesced
        } else {
            slot.pending = true;
            slot.metrics.scheduled = slot.metrics.scheduled.saturating_add(1);
            Publication::Schedule(DeliveryTicket {
                inner: Some(Arc::downgrade(&self.inner)),
            })
        }
    }

    /// Waits for any in-progress renderer before invalidating. Already queued
    /// tickets retain no payload and can deliver only this new generation.
    pub fn invalidate(&self) -> Result<ProjectionGeneration, PublicationRefusal> {
        let mut slot = lock(&self.inner);
        if slot.closed {
            return Err(PublicationRefusal::Closed);
        }
        slot.latest = None;
        match slot.generation.0.checked_add(1) {
            Some(next) => slot.generation = ProjectionGeneration(next),
            None => {
                slot.closed = true;
                return Err(PublicationRefusal::Closed);
            }
        }
        Ok(slot.generation)
    }

    pub fn close(&self) {
        let mut slot = lock(&self.inner);
        slot.closed = true;
        slot.latest = None;
    }

    pub fn metrics(&self) -> ProjectionMetrics {
        let slot = lock(&self.inner);
        ProjectionMetrics {
            pending_notifications: usize::from(slot.pending),
            ..slot.metrics
        }
    }
}

/// Queue this ticket on the UI loop once. It contains only a weak owner handle,
/// never an old projection. Dropping a failed queue submission releases the slot.
pub struct DeliveryTicket<T> {
    inner: Option<Weak<Mutex<Slot<T>>>>,
}

impl<T> DeliveryTicket<T> {
    /// The short synchronous renderer executes under the invalidation barrier.
    /// It must not re-enter this mailbox or perform blocking/async/network work.
    pub fn deliver(mut self, apply: impl FnOnce(ProjectionGeneration, T)) -> DeliveryOutcome {
        let Some(inner) = self.inner.take().and_then(|inner| inner.upgrade()) else {
            return DeliveryOutcome::OwnerGone;
        };
        let mut slot = lock(&inner);
        slot.pending = false;
        if slot.closed {
            return DeliveryOutcome::Closed;
        }
        let Some(latest) = slot.latest.take() else {
            return DeliveryOutcome::Empty;
        };
        apply(slot.generation, latest);
        slot.metrics.delivered = slot.metrics.delivered.saturating_add(1);
        DeliveryOutcome::Delivered
    }
}

impl<T> Drop for DeliveryTicket<T> {
    fn drop(&mut self) {
        if let Some(inner) = self.inner.take().and_then(|inner| inner.upgrade()) {
            let mut slot = lock(&inner);
            slot.pending = false;
            slot.latest = None;
        }
    }
}

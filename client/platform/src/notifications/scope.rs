//! Bounded native routing for one admitted session/installation generation.
//! Native callbacks carry opaque IDs; the app must still obtain a fresh server
//! grant before navigating. An OS callback never conveys authorization.
use super::*;
use std::{
    collections::{HashMap, VecDeque},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
};

pub const MAX_SCOPE_FRESHNESS: Duration = Duration::from_secs(10);
const MAX_MESSAGES: usize = 64;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ForegroundEvidence {
    NotObserved,
    AllowedByDelegate,
    SuppressedByDelegate,
}
struct Entry {
    issued_at: SystemTime,
    expires_at: SystemTime,
    retired: bool,
    opened: bool,
    foreground: ForegroundEvidence,
}
impl Entry {
    fn fresh(&self, now: SystemTime) -> bool {
        !self.retired && self.issued_at <= now && now < self.expires_at
    }
}
#[derive(Default)]
struct Book {
    entries: HashMap<PresentationId, Entry>,
    opens: VecDeque<PresentationId>,
}
pub(super) struct Inner {
    closed: AtomicBool,
    admitted_from: AtomicU64,
    admitted_until: AtomicU64,
    authority: Box<dyn Fn() -> bool + Send + Sync>,
    book: Mutex<Book>,
}
#[derive(Clone)]
pub struct PresentationScope(pub(super) Arc<Inner>);
fn millis(at: SystemTime) -> Option<u64> {
    at.duration_since(SystemTime::UNIX_EPOCH)
        .ok()?
        .as_millis()
        .try_into()
        .ok()
}
impl PresentationScope {
    /// Starts closed to presentation until refresh_until admits a fresh server
    /// read. authority must be a nonblocking, non-panicking cancellation check;
    /// it runs on native callback threads and must not acquire UI/owner locks.
    pub fn new(authority: impl Fn() -> bool + Send + Sync + 'static) -> Self {
        Self(Arc::new(Inner {
            closed: AtomicBool::new(false),
            admitted_from: AtomicU64::new(0),
            admitted_until: AtomicU64::new(0),
            authority: Box::new(authority),
            book: Mutex::new(Book::default()),
        }))
    }
    /// Only after fresh session, companion, destination and OS-permission
    /// admission. Clip the deadline to all three server-side expirations.
    /// A closed scope can never be reopened, including by a late worker.
    pub fn refresh_until(&self, until: SystemTime, now: SystemTime) -> Result<(), Error> {
        let lifetime = until.duration_since(now).map_err(|_| Error::Expired)?;
        if lifetime.is_zero() || lifetime > MAX_SCOPE_FRESHNESS {
            return Err(Error::Expired);
        }
        let _book = self.0.book.lock().map_err(|_| Error::NativeUnavailable)?;
        if self.0.closed.load(Ordering::Acquire) || !(self.0.authority)() {
            return Err(Error::ScopeClosed);
        }
        let from = millis(now).ok_or(Error::ClockUncertain)?;
        let until = millis(until).ok_or(Error::ClockUncertain)?;
        if from >= until {
            return Err(Error::Expired);
        }
        self.0.admitted_until.store(0, Ordering::Release);
        self.0.admitted_from.store(from, Ordering::Release);
        self.0.admitted_until.store(until, Ordering::Release);
        Ok(())
    }
    pub(super) fn current(&self, now: SystemTime) -> bool {
        let Some(now) = millis(now) else {
            return false;
        };
        !self.0.closed.load(Ordering::Acquire)
            && (self.0.authority)()
            && self.0.admitted_from.load(Ordering::Acquire) <= now
            && now < self.0.admitted_until.load(Ordering::Acquire)
    }
    /// Immediately disables the delegate and drains opaque IDs for targeted OS
    /// removal off the UI thread. No global remove-all operation is permitted.
    #[must_use = "remove each returned app-owned OS identifier off the UI thread"]
    pub fn close(&self) -> Vec<PresentationId> {
        self.0.closed.store(true, Ordering::Release);
        self.0.admitted_until.store(0, Ordering::Release);
        let Ok(mut book) = self.0.book.lock() else {
            return Vec::new();
        };
        book.opens.clear();
        book.entries.keys().cloned().collect()
    }
    /// Retains a tombstone for deduplication; the caller removes this ID from
    /// the OS. A late foreground callback or click cannot revive the message.
    pub fn retire(&self, id: &PresentationId) {
        if let Ok(mut book) = self.0.book.lock() {
            if let Some(entry) = book.entries.get_mut(id) {
                entry.retired = true;
            }
            book.opens.retain(|queued| queued != id);
        }
    }
    fn claim(&self, value: &Content, now: SystemTime) -> Result<(), Error> {
        value.check_fresh(now)?;
        let mut book = self.0.book.lock().map_err(|_| Error::NativeUnavailable)?;
        if !self.current(now) {
            return Err(Error::ScopeClosed);
        }
        if book.entries.contains_key(value.id()) {
            return Err(Error::AlreadyAttempted);
        }
        if book.entries.len() >= MAX_MESSAGES {
            return Err(Error::Capacity);
        }
        book.entries.insert(
            value.id().clone(),
            Entry {
                issued_at: value.issued_at,
                expires_at: value.expires_at,
                retired: false,
                opened: false,
                foreground: ForegroundEvidence::NotObserved,
            },
        );
        Ok(())
    }
    pub(super) fn dispatch<T>(
        &self,
        id: &PresentationId,
        now: SystemTime,
        send: impl FnOnce() -> T,
    ) -> Result<T, Error> {
        let book = self.0.book.lock().map_err(|_| Error::NativeUnavailable)?;
        if !self.current(now) || !book.entries.get(id).is_some_and(|e| e.fresh(now)) {
            return Err(Error::ScopeClosed);
        }
        // Serializes admission with local retirement/close without holding the
        // lock while waiting for the asynchronous completion callback.
        Ok(send())
    }
    /// One local attempt, even if settings, submission or its callback fail.
    /// The server receipt must already reserve the attempt before this call,
    /// so process restart cannot turn an uncertain result into a resend.
    pub fn submit_once(&self, value: &Content) -> Result<Submission, Error> {
        self.claim(value, SystemTime::now())?;
        #[cfg(target_os = "macos")]
        return macos::submit_once(value, self);
        #[cfg(target_os = "android")]
        return Err(Error::PendingQualification);
        #[cfg(not(any(target_os = "macos", target_os = "android")))]
        Err(Error::Unsupported)
    }
    pub fn foreground_evidence(&self, id: &PresentationId) -> ForegroundEvidence {
        self.0
            .book
            .lock()
            .ok()
            .and_then(|book| book.entries.get(id).map(|e| e.foreground))
            .unwrap_or(ForegroundEvidence::NotObserved)
    }
    pub(super) fn foreground(&self, id: &PresentationId, now: SystemTime) -> bool {
        let Ok(mut book) = self.0.book.lock() else {
            return false;
        };
        let Some(entry) = book.entries.get_mut(id) else {
            return false;
        };
        let allowed = self.current(now) && entry.fresh(now);
        entry.foreground = if allowed {
            ForegroundEvidence::AllowedByDelegate
        } else {
            ForegroundEvidence::SuppressedByDelegate
        };
        allowed
    }
    pub(super) fn opened(&self, id: &PresentationId, now: SystemTime) {
        let Ok(mut book) = self.0.book.lock() else {
            return;
        };
        if !self.current(now) {
            return;
        }
        let Some(entry) = book.entries.get_mut(id) else {
            return;
        };
        if !entry.fresh(now) || entry.opened {
            return;
        }
        entry.opened = true;
        // At most one event per claimed message, and at most 64 messages.
        book.opens.push_back(id.clone());
    }
    /// This is only an intent. Revalidate identity, TTL and target using the
    /// same ResolveOpen path as the inbox before navigating.
    pub fn take_open(&self) -> Option<PresentationId> {
        let now = SystemTime::now();
        let mut book = self.0.book.lock().ok()?;
        if !self.current(now) {
            book.opens.clear();
            return None;
        }
        while let Some(id) = book.opens.pop_front() {
            if book.entries.get(&id).is_some_and(|e| e.fresh(now)) {
                return Some(id);
            }
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn at(n: u64) -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(n)
    }
    fn value() -> Content {
        Content::new(
            PresentationId::parse("00000000000000000000000000000001").unwrap(),
            "Title".into(),
            "Body".into(),
            at(10),
            at(20),
            at(10),
        )
        .unwrap()
    }
    #[test]
    fn cancelled_generation_cannot_dispatch_or_present_and_cannot_reopen() {
        let active = Arc::new(AtomicBool::new(true));
        let authority = active.clone();
        let scope = PresentationScope::new(move || authority.load(Ordering::Acquire));
        let value = value();
        assert_eq!(scope.claim(&value, at(10)), Err(Error::ScopeClosed));
        scope.refresh_until(at(20), at(10)).unwrap();
        scope.claim(&value, at(10)).unwrap();
        assert!(scope.foreground(value.id(), at(11)));
        active.store(false, Ordering::Release);
        assert_eq!(
            scope.dispatch(value.id(), at(11), || panic!("must not dispatch")),
            Err(Error::ScopeClosed)
        );
        assert!(!scope.foreground(value.id(), at(11)));
        assert_eq!(
            scope.foreground_evidence(value.id()),
            ForegroundEvidence::SuppressedByDelegate
        );
        let ids = scope.close();
        assert_eq!(ids, vec![value.id().clone()]);
        active.store(true, Ordering::Release);
        assert_eq!(scope.refresh_until(at(20), at(11)), Err(Error::ScopeClosed));
    }
    #[test]
    fn duplicate_expired_retired_or_backward_clock_never_reappear() {
        let scope = PresentationScope::new(|| true);
        let value = value();
        scope.refresh_until(at(20), at(10)).unwrap();
        scope.claim(&value, at(10)).unwrap();
        assert_eq!(scope.claim(&value, at(11)), Err(Error::AlreadyAttempted));
        assert!(!scope.foreground(value.id(), at(9)));
        assert!(!scope.foreground(value.id(), at(20)));
        scope.opened(value.id(), at(11));
        scope.opened(value.id(), at(11));
        assert_eq!(scope.0.book.lock().unwrap().opens.len(), 1);
        scope.retire(value.id());
        assert!(scope.0.book.lock().unwrap().opens.is_empty());
        assert!(!scope.foreground(value.id(), at(11)));
        assert_eq!(scope.claim(&value, at(11)), Err(Error::AlreadyAttempted));
        assert!(scope.refresh_until(at(31), at(20)).is_err());
    }
}

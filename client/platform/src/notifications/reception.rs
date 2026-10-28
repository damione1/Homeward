//! Explicit, process-local Android foreground reception. No credentials,
//! persisted start flag, boot receiver, wake lock or automatic restart.
use super::Error;
#[cfg(target_os = "android")]
use std::sync::{Mutex, Weak, atomic::AtomicI64};
use std::{
    sync::{
        Arc,
        atomic::{AtomicU8, AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

pub const RECEPTION_IDLE_LIMIT: Duration = Duration::from_secs(15);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReceptionStatus {
    Starting,
    Running,
    Stopped,
}
struct Inner {
    #[cfg(target_os = "android")]
    id: i64,
    status: AtomicU8,
    origin: Instant,
    renewed: AtomicU64,
}
#[derive(Clone)]
pub struct ReceptionLease(Arc<Inner>);
#[cfg(target_os = "android")]
static CURRENT: Mutex<Weak<Inner>> = Mutex::new(Weak::new());
#[cfg(target_os = "android")]
static NEXT: AtomicI64 = AtomicI64::new(1);

impl ReceptionLease {
    pub fn start() -> Result<Self, Error> {
        #[cfg(not(target_os = "android"))]
        return Err(Error::Unsupported);
        #[cfg(target_os = "android")]
        {
            let mut current = CURRENT.lock().map_err(|_| Error::NativeUnavailable)?;
            if current
                .upgrade()
                .is_some_and(|inner| Self(inner).status() != ReceptionStatus::Stopped)
            {
                return Err(Error::AlreadyAttempted);
            }
            let id = NEXT
                .fetch_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
                .map_err(|_| Error::Capacity)?;
            let lease = Self(Arc::new(Inner {
                id,
                status: AtomicU8::new(0),
                origin: Instant::now(),
                renewed: AtomicU64::new(0),
            }));
            *current = Arc::downgrade(&lease.0);
            drop(current);
            if let Err(error) = super::android::start_reception(id) {
                lease.stop();
                return Err(error);
            }
            Ok(lease)
        }
    }
    pub fn status(&self) -> ReceptionStatus {
        let tick = u64::try_from(self.0.origin.elapsed().as_nanos()).ok();
        let age = tick
            .and_then(|tick| tick.checked_sub(self.0.renewed.load(Ordering::Acquire)))
            .map(Duration::from_nanos);
        let phase = self.0.status.load(Ordering::Acquire);
        match phase {
            0 if age.is_some_and(|age| age < Duration::from_secs(5)) => ReceptionStatus::Starting,
            1 if age.is_some_and(|age| age < RECEPTION_IDLE_LIMIT) => ReceptionStatus::Running,
            _ => {
                // Observed timeout is irreversible, including a late OS start
                // callback or renewal arriving after the owner stalled.
                let _ =
                    self.0
                        .status
                        .compare_exchange(phase, 2, Ordering::AcqRel, Ordering::Acquire);
                ReceptionStatus::Stopped
            }
        }
    }
    /// Only after fresh session, installation, destination and OS-permission
    /// reads. A renewal never starts or revives a stopped service.
    pub fn renew(&self) -> Result<(), Error> {
        if self.status() != ReceptionStatus::Running {
            return Err(Error::ScopeClosed);
        }
        #[cfg(not(target_os = "android"))]
        return Err(Error::Unsupported);
        #[cfg(target_os = "android")]
        {
            if let Err(error) = super::android::renew_reception(self.0.id) {
                self.stop();
                return Err(error);
            }
            if self.0.status.load(Ordering::Acquire) != 1 {
                return Err(Error::ScopeClosed);
            }
            let tick = self
                .0
                .origin
                .elapsed()
                .as_nanos()
                .try_into()
                .map_err(|_| Error::ClockUncertain)?;
            self.0.renewed.store(tick, Ordering::Release);
            Ok(())
        }
    }
    /// Revoke authority synchronously; enqueue OS cleanup without a Binder
    /// call while the application owner may hold its state lock.
    pub fn stop(&self) {
        self.0.status.store(2, Ordering::Release);
        #[cfg(target_os = "android")]
        super::android::stop_reception(self.0.id);
    }
}
impl Drop for Inner {
    fn drop(&mut self) {
        #[cfg(target_os = "android")]
        super::android::stop_reception(self.id);
    }
}
#[cfg(any(target_os = "android", test))]
impl Inner {
    fn accept_start(&self) {
        if self.status.load(Ordering::Acquire) != 0 {
            return;
        }
        let tick = u64::try_from(self.origin.elapsed().as_nanos()).unwrap_or(u64::MAX);
        if tick >= Duration::from_secs(5).as_nanos() as u64 {
            let _ = self
                .status
                .compare_exchange(0, 2, Ordering::AcqRel, Ordering::Acquire);
            return;
        }
        if self
            .status
            .compare_exchange(0, 1, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
        {
            self.renewed.store(tick, Ordering::Release);
        }
    }
}
#[cfg(target_os = "android")]
pub(super) fn changed(id: i64, running: bool) {
    let Some(inner) = CURRENT.lock().ok().and_then(|value| value.upgrade()) else {
        return;
    };
    if inner.id != id {
        return;
    }
    if running {
        inner.accept_start();
    } else {
        inner.status.store(2, Ordering::Release);
    }
}
#[cfg(target_os = "android")]
pub(super) fn destroyed() {
    if let Some(inner) = CURRENT.lock().ok().and_then(|value| value.upgrade()) {
        ReceptionLease(inner).stop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn lease(status: u8, age: Duration) -> ReceptionLease {
        ReceptionLease(Arc::new(Inner {
            #[cfg(target_os = "android")]
            id: 1,
            status: AtomicU8::new(status),
            origin: Instant::now() - age,
            renewed: AtomicU64::new(0),
        }))
    }
    #[test]
    fn a_stalled_start_or_owner_loses_its_lease_without_automatic_restart() {
        assert_eq!(
            lease(0, Duration::from_secs(6)).status(),
            ReceptionStatus::Stopped
        );
        assert_eq!(
            lease(1, Duration::from_secs(16)).status(),
            ReceptionStatus::Stopped
        );
        let active = lease(1, Duration::from_secs(1));
        assert_eq!(active.status(), ReceptionStatus::Running);
        active.stop();
        assert_eq!(active.status(), ReceptionStatus::Stopped);
        assert!(active.renew().is_err());
        let expired = lease(0, Duration::from_secs(6));
        assert_eq!(expired.status(), ReceptionStatus::Stopped);
        expired.0.accept_start();
        assert_eq!(expired.status(), ReceptionStatus::Stopped);
        let late = lease(0, Duration::from_secs(6));
        late.0.accept_start();
        assert_eq!(late.status(), ReceptionStatus::Stopped);
    }
}

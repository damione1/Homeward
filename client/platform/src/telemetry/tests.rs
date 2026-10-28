use super::*;

#[derive(Default)]
struct Probe {
    calls: usize,
}
impl Collector for Probe {
    fn battery(&mut self) -> Observation<Battery> {
        self.calls += 1;
        Observation::NoHardware
    }
    fn connectivity(&mut self) -> Observation<Connectivity> {
        self.calls += 1;
        Observation::PermissionDenied
    }
}

#[test]
fn disabled_and_suspended_never_call_the_os_or_fabricate_values() {
    let now = SystemTime::UNIX_EPOCH + Duration::from_secs(10_000);
    let mut os = Probe::default();
    let off = sample(&mut os, Consent::default(), Lifecycle::Foreground, now);
    assert_eq!(off.battery, Observation::Disabled);
    assert_eq!(off.connectivity, Observation::Disabled);
    assert_eq!(off.activity, Observation::Disabled);
    let consent = Consent {
        battery: true,
        connectivity: true,
        activity: true,
    };
    let suspended = sample(&mut os, consent, Lifecycle::Suspended, now);
    assert_eq!(suspended.battery, Observation::Stale);
    assert_eq!(suspended.connectivity, Observation::Stale);
    assert_eq!(suspended.activity, Observation::Stale);
    assert_eq!(os.calls, 0);
    let resumed = sample(&mut os, consent, Lifecycle::Foreground, now);
    assert_eq!(resumed.battery, Observation::NoHardware);
    assert_eq!(resumed.connectivity, Observation::PermissionDenied);
    assert_eq!(
        resumed.activity,
        Observation::Value(AppActivity::Foreground)
    );
    assert_eq!(os.calls, 2);
}

#[test]
fn missing_capacity_is_never_zero_and_arithmetic_is_bounded() {
    for (current, maximum) in [(0, 0), (-1, 100), (101, 100), (1, -1)] {
        assert!(BatteryPercent::from_capacity(current, maximum).is_none());
    }
    assert_eq!(BatteryPercent::from_capacity(0, 100).unwrap().get(), 0);
    assert_eq!(BatteryPercent::from_capacity(1, 3).unwrap().get(), 33);
    assert_eq!(
        BatteryPercent::from_capacity(i64::MAX, i64::MAX)
            .unwrap()
            .get(),
        100
    );
}

#[test]
fn sleep_expiry_and_clock_rollback_invalidate_freshness_without_presence_inference() {
    let now = SystemTime::UNIX_EPOCH + Duration::from_secs(10_000);
    let snapshot = sample(
        &mut Probe::default(),
        Consent::default(),
        Lifecycle::Foreground,
        now,
    );
    assert!(snapshot.is_fresh(now + Duration::from_secs(119), Cadence::Minute1));
    assert!(!snapshot.is_fresh(now + Duration::from_secs(120), Cadence::Minute1));
    assert!(!snapshot.is_fresh(now - Duration::from_secs(1), Cadence::Minute1));
}

#[test]
fn permission_and_platform_limits_do_not_advertise_unimplemented_collectors() {
    let mac = capabilities(Platform::MacOs);
    for feature in [
        Feature::Focus,
        Feature::DoNotDisturb,
        Feature::Location,
        Feature::CameraMotion,
    ] {
        let capability = mac.iter().find(|c| c.feature == feature).unwrap();
        assert_eq!(capability.availability, Availability::Unsupported);
        assert_eq!(capability.permission, Permission::Unsupported);
    }
    let activity = mac.iter().find(|c| c.feature == Feature::Activity).unwrap();
    assert_eq!(activity.availability, Availability::Partial);
    assert_eq!(activity.source, Source::NativeLifecycle);
    let android = capabilities(Platform::Android);
    assert!(
        android
            .iter()
            .all(|c| c.availability != Availability::Available)
    );
}

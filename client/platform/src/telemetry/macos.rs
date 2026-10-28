//! Public, read-only macOS SDK calls. No shell, DNS probe, SSID, host name,
//! geographic position, private Focus database or permission prompt.
use super::{Battery, BatteryPercent, Connectivity, Observation, PowerState};
use core_foundation_sys::{
    array::{CFArrayGetCount, CFArrayGetValueAtIndex, CFArrayRef},
    base::{CFEqual, CFGetTypeID, CFRelease, CFTypeRef},
    dictionary::{CFDictionaryGetTypeID, CFDictionaryGetValue, CFDictionaryRef},
    number::{
        CFBooleanGetTypeID, CFBooleanGetValue, CFBooleanRef, CFNumberGetTypeID, CFNumberGetValue,
        CFNumberRef, kCFNumberSInt64Type,
    },
    string::{CFStringCreateWithCString, CFStringGetTypeID, kCFStringEncodingUTF8},
};
use std::{
    ffi::{CStr, c_void},
    ptr,
};

unsafe extern "C" {
    fn sysctlbyname(
        name: *const std::ffi::c_char,
        value: *mut c_void,
        length: *mut usize,
        new_value: *mut c_void,
        new_length: usize,
    ) -> i32;
}
fn descriptor(name: &CStr) -> Option<String> {
    let mut bytes = [0u8; 129];
    let mut length = bytes.len();
    // SAFETY: read-only sysctl; bounded writable buffer and a valid C name.
    if unsafe {
        sysctlbyname(
            name.as_ptr(),
            bytes.as_mut_ptr().cast(),
            &mut length,
            ptr::null_mut(),
            0,
        )
    } != 0
        || !(2..=129).contains(&length)
    {
        return None;
    }
    let value = CStr::from_bytes_with_nul(&bytes[..length])
        .ok()?
        .to_str()
        .ok()?;
    if value.len() > 128 || value.chars().any(char::is_control) {
        return None;
    }
    Some(value.into())
}
pub(super) fn device_info() -> Option<super::DeviceInfo> {
    Some(super::DeviceInfo {
        platform: super::Platform::MacOs,
        model: descriptor(c"hw.model")?,
        os_version: descriptor(c"kern.osproductversion")?,
    })
}

#[link(name = "IOKit", kind = "framework")]
unsafe extern "C" {
    fn IOPSCopyPowerSourcesInfo() -> CFTypeRef;
    fn IOPSCopyPowerSourcesList(info: CFTypeRef) -> CFArrayRef;
    fn IOPSGetPowerSourceDescription(info: CFTypeRef, source: CFTypeRef) -> CFDictionaryRef;
}
#[link(name = "SystemConfiguration", kind = "framework")]
unsafe extern "C" {
    fn SCNetworkReachabilityCreateWithAddress(
        allocator: CFTypeRef,
        address: *const c_void,
    ) -> CFTypeRef;
    fn SCNetworkReachabilityGetFlags(target: CFTypeRef, flags: *mut u32) -> u8;
}

struct Owned(CFTypeRef);
impl Owned {
    fn new(value: CFTypeRef) -> Option<Self> {
        (!value.is_null()).then_some(Self(value))
    }
    fn string(value: &CStr) -> Option<Self> {
        // SAFETY: valid NUL-terminated UTF-8 literal; default allocator.
        Self::new(
            unsafe {
                CFStringCreateWithCString(ptr::null(), value.as_ptr(), kCFStringEncodingUTF8)
            }
            .cast(),
        )
    }
}
impl Drop for Owned {
    fn drop(&mut self) {
        // SAFETY: non-null Create/Copy-owned reference, released exactly once.
        unsafe { CFRelease(self.0) };
    }
}
fn field(dictionary: CFDictionaryRef, key: &CStr) -> CFTypeRef {
    let Some(key) = Owned::string(key) else {
        return ptr::null();
    };
    // SAFETY: dictionary was type-checked and remains owned by the info blob.
    unsafe { CFDictionaryGetValue(dictionary, key.0) }
}
fn number(value: CFTypeRef) -> Option<i64> {
    if value.is_null() {
        return None;
    }
    // SAFETY: type checked before conversion; output points to a live i64.
    unsafe {
        if CFGetTypeID(value) != CFNumberGetTypeID() {
            return None;
        }
        let mut result = 0i64;
        CFNumberGetValue(
            value as CFNumberRef,
            kCFNumberSInt64Type,
            (&mut result as *mut i64).cast(),
        )
        .then_some(result)
    }
}
fn boolean(value: CFTypeRef) -> Option<bool> {
    if value.is_null() {
        return None;
    }
    // SAFETY: type checked before conversion.
    unsafe {
        if CFGetTypeID(value) != CFBooleanGetTypeID() {
            return None;
        }
        Some(CFBooleanGetValue(value as CFBooleanRef))
    }
}
fn equals(value: CFTypeRef, expected: &CStr) -> bool {
    if value.is_null() {
        return false;
    }
    let Some(expected) = Owned::string(expected) else {
        return false;
    };
    // SAFETY: both references live; compare only strings.
    unsafe { CFGetTypeID(value) == CFStringGetTypeID() && CFEqual(value, expected.0) != 0 }
}

fn read_battery() -> Option<Observation<Battery>> {
    // SAFETY: SDK Copy functions return owned opaque objects. The list is
    // retained separately and every borrowed dictionary stays inside info's life.
    let info = Owned::new(unsafe { IOPSCopyPowerSourcesInfo() })?;
    let sources = Owned::new(unsafe { IOPSCopyPowerSourcesList(info.0) }.cast())?;
    // SAFETY: sources comes directly from the typed Copy API.
    let count = unsafe { CFArrayGetCount(sources.0 as CFArrayRef) };
    if !(0..=64).contains(&count) {
        return None;
    }
    for index in 0..count {
        // SAFETY: index is within the Copy API's array, info/list are alive.
        let description = unsafe {
            IOPSGetPowerSourceDescription(
                info.0,
                CFArrayGetValueAtIndex(sources.0 as CFArrayRef, index),
            )
        };
        if description.is_null() {
            return None;
        }
        // SAFETY: non-null CF object returned by the SDK.
        if unsafe { CFGetTypeID(description.cast()) != CFDictionaryGetTypeID() } {
            return None;
        }
        if !equals(field(description, c"Type"), c"InternalBattery") {
            continue;
        }
        match boolean(field(description, c"Is Present")) {
            Some(false) => continue,
            Some(true) => {}
            None => return None,
        }
        let percent = BatteryPercent::from_capacity(
            number(field(description, c"Current Capacity"))?,
            number(field(description, c"Max Capacity"))?,
        )?;
        let charging = boolean(field(description, c"Is Charging"))?;
        let power = field(description, c"Power Source State");
        let state = if charging {
            PowerState::Charging
        } else if equals(power, c"Battery Power") {
            PowerState::Battery
        } else if equals(power, c"AC Power") {
            if boolean(field(description, c"Is Charged")) == Some(true) {
                PowerState::Full
            } else {
                PowerState::ExternalPower
            }
        } else {
            return None;
        };
        return Some(Observation::Value(Battery { percent, state }));
    }
    Some(Observation::NoHardware)
}
pub(super) fn battery() -> Observation<Battery> {
    read_battery().unwrap_or(Observation::Unavailable)
}
pub(super) fn connectivity() -> Observation<Connectivity> {
    // Darwin sockaddr_in: length16, AF_INET2, port/address/padding all zero.
    // This only reads route configuration; it sends no packets and does not
    // promise Internet or Homeward availability.
    #[repr(C)]
    struct SockaddrV4 {
        length: u8,
        family: u8,
        port: u16,
        address: u32,
        padding: [u8; 8],
    }
    let address = SockaddrV4 {
        length: 16,
        family: 2,
        port: 0,
        address: 0,
        padding: [0; 8],
    };
    // SAFETY: readable sockaddr_in-compatible16bytes, copied by SDK call.
    let Some(target) = Owned::new(unsafe {
        SCNetworkReachabilityCreateWithAddress(ptr::null(), (&address as *const SockaddrV4).cast())
    }) else {
        return Observation::Unavailable;
    };
    let mut flags = 0u32;
    // SAFETY: retained target reference and writable aligned flags.
    if unsafe { SCNetworkReachabilityGetFlags(target.0, &mut flags) } == 0 {
        return Observation::Unavailable;
    }
    Observation::Value(if flags & (1 << 1) == 0 {
        Connectivity::NoLocalRoute
    } else if flags & (1 << 2) != 0 {
        Connectivity::ConnectionRequired
    } else {
        Connectivity::LocallyReachable
    })
}

//! Read-only Android SDK observations through the shared bounded JNI worker.
use super::*;
use crate::android::host;
use jni::objects::{JIntArray, JObjectArray, JString};

pub(super) fn device_info() -> Option<DeviceInfo> {
    host::call(|context| {
        if !context.is_alive() {
            return Err(host::Failure::Closed);
        }
        context.with_env(|env| {
            let raw = env
                .call_method(
                    context.activity.as_obj(),
                    "companionDeviceInfo",
                    "()[Ljava/lang/String;",
                    &[],
                )?
                .l()?;
            let values = JObjectArray::from(raw);
            if env.get_array_length(&values)? != 2 {
                return Ok(None);
            }
            let mut strings = [String::new(), String::new()];
            for (index, value) in strings.iter_mut().enumerate() {
                let raw = JString::from(env.get_object_array_element(&values, index as i32)?);
                if raw.is_null()
                    || !(1..=128).contains(&env.call_method(&raw, "length", "()I", &[])?.i()?)
                {
                    return Ok(None);
                }
                *value = env.get_string(&raw)?.into();
                if value.len() > 128 || value.trim() != value || value.chars().any(char::is_control)
                {
                    return Ok(None);
                }
            }
            let [model, os_version] = strings;
            Ok(Some(DeviceInfo {
                platform: Platform::Android,
                model,
                os_version,
            }))
        })
    })
    .ok()
    .flatten()
}
pub(super) fn battery() -> Observation<Battery> {
    let raw = host::call(|context| {
        if !context.is_alive() {
            return Err(host::Failure::Closed);
        }
        context.with_env(|env| {
            let raw = env
                .call_method(context.activity.as_obj(), "companionBattery", "()[I", &[])?
                .l()?;
            let values = JIntArray::from(raw);
            if env.get_array_length(&values)? != 5 {
                return Ok(None);
            }
            let mut raw = [0; 5];
            env.get_int_array_region(&values, 0, &mut raw)?;
            Ok(Some(raw))
        })
    });
    raw.ok()
        .flatten()
        .map_or(Observation::Unavailable, android_values::battery)
}
pub(super) fn connectivity() -> Observation<Connectivity> {
    let raw = host::call(|context| {
        if !context.is_alive() {
            return Err(host::Failure::Closed);
        }
        context.with_env(|env| {
            env.call_method(
                context.activity.as_obj(),
                "companionConnectivity",
                "()I",
                &[],
            )?
            .i()
        })
    });
    raw.map_or(Observation::Unavailable, android_values::connectivity)
}

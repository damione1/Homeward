//! Native chrome follows the application's explicit light/dark preference.
#[derive(Clone, Copy)]
pub enum SystemBars {
    Light,
    Dark,
}
pub fn apply(value: SystemBars) {
    #[cfg(target_os = "android")]
    {
        use crate::android::host;
        let _ = host::enqueue(move |context| {
            if !context.is_alive() {
                return Err(host::Failure::Closed);
            }
            context.with_env(|env| {
                env.call_method(
                    context.activity.as_obj(),
                    "systemBarAppearance",
                    "(Z)V",
                    &[jni::objects::JValue::Bool(
                        matches!(value, SystemBars::Dark).into(),
                    )],
                )?;
                Ok(())
            })
        });
    }
    #[cfg(not(target_os = "android"))]
    let _ = value;
}

fn main() {
    // Slint Kit 0.8.0 keeps Slint's debug metadata default. The app's unit
    // tests inspect elements by component identity, so unoptimized builds opt
    // in. Release builds keep the default, where SLINT_EMIT_DEBUG_INFO applies.
    let config = slint_kit_pro_build::config();
    let config = if std::env::var("PROFILE").as_deref() == Ok("debug") {
        config.with_debug_info(true)
    } else {
        config
    };
    slint_kit_pro_build::compile_with_config("ui/app.slint", config)
        .expect("compile Homeward native shell");
}

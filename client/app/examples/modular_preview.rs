//! Explicit synthetic native preview; never loads a saved product profile.
pub use homeward_app::*;
use slint::ComponentHandle;
#[path = "fixtures/modular.rs"]
mod fixture;
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let window = HomewardWindow::new()?;
    slint_kit_pro::install(&window);
    fixture::apply(&window, std::env::args().any(|arg| arg == "--detail"));
    window.run()?;
    Ok(())
}

//! Opening the window without a GPU.
//!
//! The app draws with Vulkan (wgpu, through Rune). A machine with no GPU still draws, slower, on
//! a software Vulkan driver (Mesa's lavapipe), which Rune takes like any other adapter. A machine
//! with no Vulkan driver at all cannot draw: Rune records why the window could not be made and
//! leaves the app running windowless, which under `discobox desktop` is a process that never
//! shows anything. `watch` turns that into a plain message and an exit, which the CLI passes on.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

/// What installs a software Vulkan driver, said when there is none to draw with.
pub const INSTALL: &str = "the app draws with Vulkan; without a GPU it draws in software on Mesa's Vulkan driver. \
Install it and the Vulkan loader: on Debian and Ubuntu mesa-vulkan-drivers and libvulkan1, \
on Fedora mesa-vulkan-drivers and vulkan-loader, on Arch vulkan-swrast and vulkan-icd-loader";

/// Why the window could not be made, plainly: `error` is what Rune or wgpu said, `adapters` how
/// many Vulkan adapters (hardware or software) this machine has.
pub fn explain(error: &str, adapters: usize) -> String {
    if adapters == 0 {
        format!("discobox desktop: no graphics driver to draw the window with: {INSTALL} ({error})")
    } else {
        format!("discobox desktop: could not open the window: {error}")
    }
}

/// How many adapters wgpu finds, on the instance Rune draws with. Only asked after Rune's own GPU
/// work has finished (its window failed, or there is no window): Vulkan loaders crash when two
/// threads make instances or devices at once.
pub fn adapters() -> usize {
    let instance = rune::render::Gpu::instance();
    pollster::block_on(instance.enumerate_adapters(wgpu::Backends::all())).len()
}

/// Watches the window being made, from another thread: once it opens, nothing; if Rune says it
/// could not be made first, says why and exits, since a windowless app is of no use to anyone.
/// `opened` is set by the app's `on_window`.
pub fn watch<S: 'static>(handle: rune::AppHandle<S>, opened: Arc<AtomicBool>) {
    std::thread::Builder::new()
        .name("window-watch".into())
        .spawn(move || {
            loop {
                if opened.load(Ordering::Acquire) {
                    return;
                }
                if let Some(error) = handle.window_error() {
                    eprintln!("{}", explain(&error, adapters()));
                    std::process::exit(1);
                }
                std::thread::sleep(Duration::from_millis(50));
            }
        })
        .expect("spawn the window watch");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_driver_says_what_to_install() {
        let m = explain("Failed to create surface for any enabled backend: {}", 0);
        assert!(m.contains("no graphics driver"), "{m}");
        assert!(m.contains("mesa-vulkan-drivers") && m.contains("vulkan-swrast"), "{m}");
        assert!(m.ends_with("(Failed to create surface for any enabled backend: {})"), "what wgpu said is kept: {m}");
    }

    #[test]
    fn a_driver_that_fails_is_not_told_to_install_one() {
        let m = explain("no GPU device: out of memory", 1);
        assert_eq!(m, "discobox desktop: could not open the window: no GPU device: out of memory");
    }
}

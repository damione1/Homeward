//! Bounded compressed video and native decoding. No network, authority or Slint objects.
//! The application must own the decoder on its blocking worker and retire it with
//! the authorized session. A decoded picture is not evidence of authorization.
use zeroize::Zeroizing;

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
pub use macos::H264Decoder;

pub use home_client_model::video::{
    EncodedFrame, FrameKind, H264Format, InputFailure, MAX_HEIGHT, MAX_WIDTH,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DecodeFailure {
    Input(InputFailure),
    DimensionLimit,
    KeyframeRequired,
    HardwareUnavailable,
    Native(i32),
    MissingOutput,
    InvalidOutput,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DecoderBackend {
    VideoToolboxHardware,
}

/// Private owned RGBA. No Debug/serialization/disk cache; cleared on drop.
pub struct DecodedFrame {
    width: u32,
    height: u32,
    pixels: Zeroizing<Vec<u8>>,
}
impl DecodedFrame {
    pub fn dimensions(&self) -> (u32, u32) {
        (self.width, self.height)
    }
    pub fn pixels(&self) -> &[u8] {
        &self.pixels
    }
}

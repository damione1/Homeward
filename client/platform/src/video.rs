//! Bounded compressed video and native decoding. No network, authority or Slint objects.
//! The application must own the decoder on its blocking worker and retire it with
//! the authorized session. A decoded picture is not evidence of authorization.
use zeroize::Zeroizing;

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
pub use macos::H264Decoder;

// Desktop-first qualification currently supplies VideoToolbox. Other targets
// compile the SAME reader and explicitly refuse native decoding; no software
// decoder or fabricated picture is substituted. Android MediaCodec is still
// part of the final023–024 adaptation/physical qualification.
#[cfg(not(target_os = "macos"))]
pub struct H264Decoder(std::convert::Infallible);
#[cfg(not(target_os = "macos"))]
impl H264Decoder {
    pub fn new(_format: &H264Format) -> Result<Self, DecodeFailure> {
        Err(DecodeFailure::HardwareUnavailable)
    }
    pub fn backend(&self) -> DecoderBackend {
        match self.0 {}
    }
    pub fn dimensions(&self) -> (u32, u32) {
        match self.0 {}
    }
    pub fn decode(&mut self, _frame: &EncodedFrame) -> Result<DecodedFrame, DecodeFailure> {
        match self.0 {}
    }
}

pub use home_client_model::video::{
    EncodedFrame, FrameKind, H264Format, InputFailure, MAX_HEIGHT, MAX_WIDTH,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DecodeFailure {
    Input(InputFailure),
    DimensionLimit,
    KeyframeRequired,
    HardwareUnavailable,
    Format(i32),
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

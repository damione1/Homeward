//! Bounded compressed video and native decoding. No network, authority or Slint objects.
//! The application must own the decoder on its blocking worker and retire it with
//! the authorized session. A decoded picture is not evidence of authorization.
use zeroize::Zeroizing;

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
pub use macos::H264Decoder;

pub const MAX_PARAMETER_BYTES: usize = 1024;
pub const MAX_SAMPLE_BYTES: usize = 512 * 1024;
pub const MAX_NALS_PER_SAMPLE: usize = 64;
pub const MAX_WIDTH: u32 = 1920;
pub const MAX_HEIGHT: u32 = 1080;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DecodeFailure {
    InvalidParameters,
    UnsupportedProfile,
    InvalidSample,
    SampleLimit,
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

/// Raw SPS/PPS NAL units, without Annex-B start codes or AVCC lengths.
/// The first qualification profile is H.264 baseline (no reordered B frames).
pub struct H264Format {
    sps: Zeroizing<Vec<u8>>,
    pps: Zeroizing<Vec<u8>>,
}
impl H264Format {
    pub fn new(sps: Vec<u8>, pps: Vec<u8>) -> Result<Self, DecodeFailure> {
        let sps = Zeroizing::new(sps);
        let pps = Zeroizing::new(pps);
        if !(4..=MAX_PARAMETER_BYTES).contains(&sps.len())
            || !(2..=MAX_PARAMETER_BYTES).contains(&pps.len())
            || sps[0] & 0x9f != 7
            || pps[0] & 0x9f != 8
        {
            return Err(DecodeFailure::InvalidParameters);
        }
        if sps[1] != 66 {
            return Err(DecodeFailure::UnsupportedProfile);
        }
        Ok(Self { sps, pps })
    }
    pub fn sps(&self) -> &[u8] {
        &self.sps
    }
    pub fn pps(&self) -> &[u8] {
        &self.pps
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum FrameKind {
    Keyframe,
    Predicted,
}

/// Exactly one access unit in four-byte-length AVCC format, with at least
/// one VCL NAL. Parameter changes require a new decoder, never an in-band
/// dimension change. Sequence and session identity are checked by the host.
pub struct EncodedFrame {
    bytes: Zeroizing<Vec<u8>>,
    presentation_micros: i64,
    kind: FrameKind,
}
impl EncodedFrame {
    pub fn new(bytes: Vec<u8>, presentation_micros: u64) -> Result<Self, DecodeFailure> {
        let bytes = Zeroizing::new(bytes);
        if bytes.len() > MAX_SAMPLE_BYTES {
            return Err(DecodeFailure::SampleLimit);
        }
        let presentation_micros =
            i64::try_from(presentation_micros).map_err(|_| DecodeFailure::InvalidSample)?;
        let mut tail = bytes.as_slice();
        let mut kind = None;
        let mut count = 0;
        while !tail.is_empty() {
            count += 1;
            if count > MAX_NALS_PER_SAMPLE {
                return Err(DecodeFailure::SampleLimit);
            }
            let length = tail.get(..4).ok_or(DecodeFailure::InvalidSample)?;
            let length = u32::from_be_bytes(length.try_into().unwrap()) as usize;
            tail = &tail[4..];
            let nal = tail.get(..length).ok_or(DecodeFailure::InvalidSample)?;
            let header = *nal.first().ok_or(DecodeFailure::InvalidSample)?;
            if header & 0x80 != 0 {
                return Err(DecodeFailure::InvalidSample);
            }
            let next = match header & 31 {
                1 => Some(FrameKind::Predicted),
                5 => Some(FrameKind::Keyframe),
                6 | 9 => None, // Supplemental information / access-unit delimiter.
                _ => return Err(DecodeFailure::InvalidSample), // Open H.264 NAL codes.
            };
            if let Some(next) = next {
                if kind.is_some_and(|current| current != next) {
                    return Err(DecodeFailure::InvalidSample);
                }
                kind = Some(next);
            }
            tail = &tail[length..];
        }
        Ok(Self {
            bytes,
            presentation_micros,
            kind: kind.ok_or(DecodeFailure::InvalidSample)?,
        })
    }
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }
    pub fn presentation_micros(&self) -> i64 {
        self.presentation_micros
    }
    pub fn kind(&self) -> FrameKind {
        self.kind
    }
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

#[cfg(test)]
mod tests {
    use super::*;

    fn sample(nals: &[&[u8]]) -> Vec<u8> {
        let mut result = Vec::new();
        for nal in nals {
            result.extend_from_slice(&(nal.len() as u32).to_be_bytes());
            result.extend_from_slice(nal);
        }
        result
    }

    #[test]
    fn admission_rejects_unbounded_malformed_or_renegotiated_frames() {
        for bytes in [
            vec![],
            vec![0, 0, 0],
            vec![0, 0, 0, 4, 0x65],
            sample(&[&[]]),
            sample(&[&[0xe5, 0]]),
            sample(&[&[0x67, 66, 0, 0]]),
            sample(&[&[0x65, 0], &[0x41, 0]]),
            sample(&[&[0x09, 0]]),
        ] {
            assert!(matches!(
                EncodedFrame::new(bytes, 0),
                Err(DecodeFailure::InvalidSample)
            ));
        }
        assert!(matches!(
            EncodedFrame::new(vec![0; MAX_SAMPLE_BYTES + 1], 0),
            Err(DecodeFailure::SampleLimit)
        ));
        let too_many = vec![&[0x09, 0][..]; MAX_NALS_PER_SAMPLE];
        let mut bytes = sample(&too_many);
        bytes.extend(sample(&[&[0x65, 0]]));
        assert!(matches!(
            EncodedFrame::new(bytes, 0),
            Err(DecodeFailure::SampleLimit)
        ));
        assert!(matches!(
            EncodedFrame::new(sample(&[&[0x65, 0]]), u64::MAX),
            Err(DecodeFailure::InvalidSample)
        ));
    }

    #[test]
    fn keyframes_and_predictions_keep_exact_bytes_and_timestamp() {
        for (nal, expected) in [(0x65, FrameKind::Keyframe), (0x41, FrameKind::Predicted)] {
            let bytes = sample(&[&[9, 0xf0], &[nal, 1, 2, 3]]);
            let frame = EncodedFrame::new(bytes.clone(), 123_456).unwrap();
            assert_eq!(frame.bytes(), bytes);
            assert_eq!(frame.presentation_micros(), 123_456);
            assert_eq!(frame.kind(), expected);
        }
    }

    #[test]
    fn format_requires_bounded_baseline_sps_and_pps() {
        assert!(H264Format::new(vec![0x67, 66, 0, 31], vec![0x68, 0]).is_ok());
        assert!(matches!(
            H264Format::new(vec![0x67, 100, 0, 31], vec![0x68, 0]),
            Err(DecodeFailure::UnsupportedProfile)
        ));
        for sps in [
            vec![],
            vec![0xe7, 66, 0, 31],
            vec![0x67; MAX_PARAMETER_BYTES + 1],
        ] {
            assert!(matches!(
                H264Format::new(sps, vec![0x68, 0]),
                Err(DecodeFailure::InvalidParameters)
            ));
        }
    }
}

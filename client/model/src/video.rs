//! Bounded H.264 baseline access units; independent of transport and decoder.
use zeroize::Zeroizing;

pub const MAX_PARAMETER_BYTES: usize = 1024;
pub const MAX_SAMPLE_BYTES: usize = 512 * 1024;
pub const MAX_NALS_PER_SAMPLE: usize = 64;
pub const MAX_WIDTH: u32 = 1920;
pub const MAX_HEIGHT: u32 = 1080;
pub const MAX_FRAMES_PER_SECOND: u32 = 30;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InputFailure {
    InvalidParameters,
    UnsupportedProfile,
    InvalidSample,
    SampleLimit,
    DimensionLimit,
    FrameRateLimit,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VideoDimensions {
    width: u32,
    height: u32,
}
impl VideoDimensions {
    pub fn new(width: u32, height: u32) -> Result<Self, InputFailure> {
        if width == 0 || height == 0 || width > MAX_WIDTH || height > MAX_HEIGHT {
            return Err(InputFailure::DimensionLimit);
        }
        Ok(Self { width, height })
    }
    pub fn width(self) -> u32 {
        self.width
    }
    pub fn height(self) -> u32 {
        self.height
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct FrameRate(u32);
impl FrameRate {
    pub fn new(value: u32) -> Result<Self, InputFailure> {
        if value == 0 || value > MAX_FRAMES_PER_SECOND {
            return Err(InputFailure::FrameRateLimit);
        }
        Ok(Self(value))
    }
    pub fn get(self) -> u32 {
        self.0
    }
}

/// Raw SPS/PPS NAL units, without Annex-B start codes or AVCC lengths.
/// The first qualification profile is H.264 baseline (no reordered B frames).
pub struct H264Format {
    sps: Zeroizing<Vec<u8>>,
    pps: Zeroizing<Vec<u8>>,
}
impl H264Format {
    pub fn new(sps: Vec<u8>, pps: Vec<u8>) -> Result<Self, InputFailure> {
        let sps = Zeroizing::new(sps);
        let pps = Zeroizing::new(pps);
        if !(4..=MAX_PARAMETER_BYTES).contains(&sps.len())
            || !(2..=MAX_PARAMETER_BYTES).contains(&pps.len())
            || sps[0] & 0x9f != 7
            || pps[0] & 0x9f != 8
        {
            return Err(InputFailure::InvalidParameters);
        }
        if sps[1] != 66 {
            return Err(InputFailure::UnsupportedProfile);
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
    pub fn new(bytes: Vec<u8>, presentation_micros: u64) -> Result<Self, InputFailure> {
        let bytes = Zeroizing::new(bytes);
        if bytes.len() > MAX_SAMPLE_BYTES {
            return Err(InputFailure::SampleLimit);
        }
        let presentation_micros =
            i64::try_from(presentation_micros).map_err(|_| InputFailure::InvalidSample)?;
        let mut tail = bytes.as_slice();
        let mut kind = None;
        let mut count = 0;
        while !tail.is_empty() {
            count += 1;
            if count > MAX_NALS_PER_SAMPLE {
                return Err(InputFailure::SampleLimit);
            }
            let length = tail.get(..4).ok_or(InputFailure::InvalidSample)?;
            let length = u32::from_be_bytes(length.try_into().unwrap()) as usize;
            tail = &tail[4..];
            let nal = tail.get(..length).ok_or(InputFailure::InvalidSample)?;
            let header = *nal.first().ok_or(InputFailure::InvalidSample)?;
            if header & 0x80 != 0 {
                return Err(InputFailure::InvalidSample);
            }
            let next = match header & 31 {
                1 => Some(FrameKind::Predicted),
                5 => Some(FrameKind::Keyframe),
                6 | 9 => None, // Supplemental information / access-unit delimiter.
                _ => return Err(InputFailure::InvalidSample), // Open H.264 NAL codes.
            };
            if let Some(next) = next {
                if kind.is_some_and(|current| current != next) {
                    return Err(InputFailure::InvalidSample);
                }
                kind = Some(next);
            }
            tail = &tail[length..];
        }
        Ok(Self {
            bytes,
            presentation_micros,
            kind: kind.ok_or(InputFailure::InvalidSample)?,
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
                Err(InputFailure::InvalidSample)
            ));
        }
        assert!(matches!(
            EncodedFrame::new(vec![0; MAX_SAMPLE_BYTES + 1], 0),
            Err(InputFailure::SampleLimit)
        ));
        let too_many = vec![&[0x09, 0][..]; MAX_NALS_PER_SAMPLE];
        let mut bytes = sample(&too_many);
        bytes.extend(sample(&[&[0x65, 0]]));
        assert!(matches!(
            EncodedFrame::new(bytes, 0),
            Err(InputFailure::SampleLimit)
        ));
        assert!(matches!(
            EncodedFrame::new(sample(&[&[0x65, 0]]), u64::MAX),
            Err(InputFailure::InvalidSample)
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
            Err(InputFailure::UnsupportedProfile)
        ));
        for sps in [
            vec![],
            vec![0xe7, 66, 0, 31],
            vec![0x67; MAX_PARAMETER_BYTES + 1],
        ] {
            assert!(matches!(
                H264Format::new(sps, vec![0x68, 0]),
                Err(InputFailure::InvalidParameters)
            ));
        }
    }
}

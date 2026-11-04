//! One current caller-bound MediaSession stream. A decoder header cannot renew
//! the lease; samples retain their exact session and original microsecond offset.
use crate::{
    camera_content::{self as camera, AdmissionError, Lease},
    content_contract,
    home::api::v2 as wire,
};
use buffa::{EnumValue, Message};
use home_client_model::{
    api_content::ContentKind,
    video::{EncodedFrame, FrameKind, FrameRate, H264Format, VideoDimensions},
};
use std::time::Instant;
pub struct Header {
    pub format: H264Format,
    pub dimensions: VideoDimensions,
    pub rate: FrameRate,
}
pub enum VideoFrame {
    Header(Header),
    Sample(EncodedFrame),
}
enum Phase {
    AwaitingHeader,
    Streaming { last_timestamp: Option<u64> },
    Ended,
}
pub struct VideoStream {
    lease: Lease,
    sequence: u64,
    phase: Phase,
}
impl VideoStream {
    pub fn new(lease: Lease) -> Self {
        Self {
            lease,
            sequence: 0,
            phase: Phase::AwaitingHeader,
        }
    }
    pub fn accept(
        &mut self,
        value: wire::StreamMediaSessionResponse,
        now: Instant,
    ) -> Result<VideoFrame, AdmissionError> {
        let phase = std::mem::replace(&mut self.phase, Phase::Ended);
        if matches!(phase, Phase::Ended) {
            return Err(AdmissionError::Sequence);
        }
        self.lease.live(now)?;
        if self.lease.session().kind() != ContentKind::Camera
            || u64::try_from(value.sequence).ok() != self.sequence.checked_add(1)
            || !value.__buffa_unknown_fields.is_empty()
            || value.encoded_len() > 1024 * 1024
        {
            return Err(AdmissionError::Sequence);
        }
        self.sequence = self
            .sequence
            .checked_add(1)
            .ok_or(AdmissionError::Sequence)?;
        match value.frame.ok_or(AdmissionError::Invalid)? {
            wire::stream_media_session_response::Frame::Header(value) => {
                if !matches!(phase, Phase::AwaitingHeader) {
                    return Err(AdmissionError::Sequence);
                }
                let header = self.header(*value)?;
                self.phase = Phase::Streaming {
                    last_timestamp: None,
                };
                Ok(VideoFrame::Header(header))
            }
            wire::stream_media_session_response::Frame::Sample(value) => {
                let Phase::Streaming { last_timestamp } = phase else {
                    return Err(AdmissionError::Sequence);
                };
                if !value.__buffa_unknown_fields.is_empty()
                    || value.media_session != self.lease.name().as_str()
                {
                    return Err(AdmissionError::ForeignResource);
                }
                let stamp = value
                    .presentation_duration
                    .as_option()
                    .ok_or(AdmissionError::Invalid)?;
                if !stamp.__buffa_unknown_fields.is_empty()
                    || !(0..=60).contains(&stamp.seconds)
                    || !(0..1_000_000_000).contains(&stamp.nanos)
                    || stamp.nanos % 1000 != 0
                {
                    return Err(AdmissionError::Invalid);
                }
                let stamp = (stamp.seconds as u64) * 1_000_000 + stamp.nanos as u64 / 1000;
                if stamp > camera::MAX_LEASE.as_micros() as u64
                    || last_timestamp.is_some_and(|last| stamp <= last)
                {
                    return Err(AdmissionError::Sequence);
                }
                let sample =
                    EncodedFrame::new(value.avcc, stamp).map_err(|_| AdmissionError::Invalid)?;
                if last_timestamp.is_none() && sample.kind() != FrameKind::Keyframe {
                    return Err(AdmissionError::Sequence);
                }
                self.phase = Phase::Streaming {
                    last_timestamp: Some(stamp),
                };
                Ok(VideoFrame::Sample(sample))
            }
        }
    }
    fn header(&self, value: wire::MediaVideoHeader) -> Result<Header, AdmissionError> {
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(AdmissionError::Invalid);
        }
        let session = content_contract::session(
            value
                .media_session
                .as_option()
                .ok_or(AdmissionError::Invalid)?,
        )
        .map_err(|_| AdmissionError::Invalid)?;
        if !self.lease.session().same_lease(&session) {
            return Err(AdmissionError::ForeignResource);
        }
        match value.codec {
            EnumValue::Known(wire::MediaVideoCodec::H264Baseline) => {}
            EnumValue::Known(wire::MediaVideoCodec::Unspecified) | EnumValue::Unknown(_) => {
                return Err(AdmissionError::Invalid);
            }
        }
        let width = u32::try_from(value.width).map_err(|_| AdmissionError::Capacity)?;
        let height = u32::try_from(value.height).map_err(|_| AdmissionError::Capacity)?;
        let rate = u32::try_from(value.frames_per_second).map_err(|_| AdmissionError::Capacity)?;
        Ok(Header {
            format: H264Format::new(value.sps, value.pps).map_err(|_| AdmissionError::Invalid)?,
            dimensions: VideoDimensions::new(width, height)
                .map_err(|_| AdmissionError::Capacity)?,
            rate: FrameRate::new(rate).map_err(|_| AdmissionError::Capacity)?,
        })
    }
}

//! Stateful admission for one camera-scoped baseline-H.264 stream.
//! Header never extends the original monotonic lease; failures retire admission.
use crate::{
    cameras_contract::{self as camera, AdmissionError, Lease},
    home::cameras::v1 as wire,
};
use buffa::{EnumValue, Message};
use home_client_model::video::{EncodedFrame, FrameKind, FrameRate, H264Format, VideoDimensions};
use std::time::Instant;

pub struct Header {
    pub format: H264Format,
    pub dimensions: VideoDimensions,
    pub rate: FrameRate,
}
pub enum VideoFrame {
    Header(Header),
    Sample(EncodedFrame),
    Expired,
    Revoked,
    Unavailable,
    Unsupported,
    ResyncRequired,
}
enum Phase {
    AwaitingHeader,
    Streaming {
        last_timestamp: Option<u64>,
        rate: FrameRate,
    },
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
        value: wire::ReadVideoResponse,
        now: Instant,
    ) -> Result<VideoFrame, AdmissionError> {
        let phase = std::mem::replace(&mut self.phase, Phase::Ended);
        match phase {
            Phase::Ended => return Err(AdmissionError::Sequence),
            Phase::AwaitingHeader | Phase::Streaming { .. } => {}
        }
        self.lease.live(now)?;
        let next = self
            .sequence
            .checked_add(1)
            .ok_or(AdmissionError::Sequence)?;
        if value.sequence != next
            || !value.__buffa_unknown_fields.is_empty()
            || value.encoded_len() > 1024 * 1024
        {
            return Err(AdmissionError::Sequence);
        }
        self.sequence = next;
        let frame = value.frame.ok_or(AdmissionError::Invalid)?;
        use wire::read_video_response::Frame;
        let result = match frame {
            Frame::Header(value) => {
                match phase {
                    Phase::AwaitingHeader => {}
                    Phase::Streaming { .. } | Phase::Ended => return Err(AdmissionError::Sequence),
                }
                let header = self.header(*value)?;
                self.phase = Phase::Streaming {
                    last_timestamp: None,
                    rate: header.rate,
                };
                VideoFrame::Header(header)
            }
            Frame::Sample(value) => {
                let (last_timestamp, rate) = match phase {
                    Phase::Streaming {
                        last_timestamp,
                        rate,
                    } => (last_timestamp, rate),
                    Phase::AwaitingHeader | Phase::Ended => return Err(AdmissionError::Sequence),
                };
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(AdmissionError::Invalid);
                }
                self.same_reference(value.session.as_option())?;
                let stamp = value.presentation_micros;
                if stamp > camera::MAX_LEASE.as_micros() as u64
                    || last_timestamp.is_some_and(|last| {
                        stamp <= last || stamp - last < 1_000_000 / u64::from(rate.get())
                    })
                {
                    return Err(AdmissionError::Sequence);
                }
                let sample =
                    EncodedFrame::new(value.avcc, stamp).map_err(|_| AdmissionError::Invalid)?;
                if last_timestamp.is_none() {
                    match sample.kind() {
                        FrameKind::Keyframe => {}
                        FrameKind::Predicted => return Err(AdmissionError::Sequence),
                    }
                }
                self.phase = Phase::Streaming {
                    last_timestamp: Some(stamp),
                    rate,
                };
                VideoFrame::Sample(sample)
            }
            Frame::Expired(value) => {
                clean_empty(&value)?;
                VideoFrame::Expired
            }
            Frame::Revoked(value) => {
                clean_empty(&value)?;
                VideoFrame::Revoked
            }
            Frame::Unavailable(value) => {
                clean_empty(&value)?;
                VideoFrame::Unavailable
            }
            Frame::Unsupported(value) => {
                clean_empty(&value)?;
                VideoFrame::Unsupported
            }
            Frame::ResyncRequired(value) => {
                clean_empty(&value)?;
                VideoFrame::ResyncRequired
            }
        };
        Ok(result)
    }
    fn same_reference(&self, value: Option<&wire::SessionRef>) -> Result<(), AdmissionError> {
        let reference = camera::session_reference(value.ok_or(AdmissionError::Invalid)?)?;
        if &reference != self.lease.reference() {
            return Err(AdmissionError::ForeignResource);
        }
        Ok(())
    }
    fn header(&self, value: wire::VideoHeader) -> Result<Header, AdmissionError> {
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(AdmissionError::Invalid);
        }
        let lease = value.lease.as_option().ok_or(AdmissionError::Invalid)?;
        if !lease.__buffa_unknown_fields.is_empty() {
            return Err(AdmissionError::Invalid);
        }
        self.same_reference(lease.session.as_option())?;
        let issued =
            camera::timestamp(lease.issued_at.as_option().ok_or(AdmissionError::Invalid)?)?;
        let expires = camera::timestamp(
            lease
                .expires_at
                .as_option()
                .ok_or(AdmissionError::Invalid)?,
        )?;
        if issued != self.lease.issued_at()
            || expires != self.lease.expires_at()
            || lease.event_revision != self.lease.revision()
        {
            return Err(AdmissionError::ForeignResource);
        }
        match value.codec {
            EnumValue::Known(wire::VideoCodec::H264Baseline) => {}
            EnumValue::Known(wire::VideoCodec::Unspecified) | EnumValue::Unknown(_) => {
                return Err(AdmissionError::Invalid);
            }
        }
        Ok(Header {
            format: H264Format::new(value.sps, value.pps).map_err(|_| AdmissionError::Invalid)?,
            dimensions: VideoDimensions::new(value.width, value.height)
                .map_err(|_| AdmissionError::Capacity)?,
            rate: FrameRate::new(value.frames_per_second).map_err(|_| AdmissionError::Capacity)?,
        })
    }
}
fn clean_empty(value: &buffa_types::google::protobuf::Empty) -> Result<(), AdmissionError> {
    if value.__buffa_unknown_fields.is_empty() {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

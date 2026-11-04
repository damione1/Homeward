use buffa_types::google::protobuf::{Duration as WireDuration, Timestamp};
use home_client_model::{
    api_content::{ContentKind, MediaSourceName},
    video::FrameKind,
};
use home_wire_contract::{
    camera_content as camera,
    home::api::v2 as wire,
    media_video_contract::{VideoFrame, VideoStream},
};
use std::time::{Duration, Instant, UNIX_EPOCH};
fn source() -> MediaSourceName {
    format!("homes/synthetic/mediaSources/source-{}", "a".repeat(48))
        .parse()
        .unwrap()
}
fn raw_session() -> wire::MediaSession {
    wire::MediaSession {
        name: format!("homes/synthetic/mediaSessions/s-{}", "a".repeat(52)),
        media_source: source().as_str().to_owned(),
        content_kind: wire::MediaContentKind::Camera.into(),
        create_time: Timestamp {
            seconds: 100,
            ..Default::default()
        }
        .into(),
        expire_time: Timestamp {
            seconds: 160,
            ..Default::default()
        }
        .into(),
        events_etag: "\"opaque-events\"".to_owned(),
        ..Default::default()
    }
}
fn lease() -> camera::Lease {
    camera::lease(
        &raw_session(),
        &source(),
        ContentKind::Camera,
        UNIX_EPOCH + Duration::from_secs(101),
        Instant::now(),
    )
    .unwrap()
}
fn header() -> wire::StreamMediaSessionResponse {
    wire::StreamMediaSessionResponse {
        sequence: 1,
        frame: Some(wire::stream_media_session_response::Frame::Header(
            Box::new(wire::MediaVideoHeader {
                media_session: raw_session().into(),
                codec: wire::MediaVideoCodec::H264Baseline.into(),
                sps: vec![0x67, 66, 0, 30],
                pps: vec![0x68, 0],
                width: 320,
                height: 240,
                frames_per_second: 30,
                ..Default::default()
            }),
        )),
        ..Default::default()
    }
}
fn sample(sequence: i64, micros: i64, nal: u8) -> wire::StreamMediaSessionResponse {
    wire::StreamMediaSessionResponse {
        sequence,
        frame: Some(wire::stream_media_session_response::Frame::Sample(
            Box::new(wire::MediaVideoSample {
                media_session: raw_session().name,
                presentation_duration: WireDuration {
                    seconds: micros / 1_000_000,
                    nanos: ((micros % 1_000_000) * 1000) as i32,
                    ..Default::default()
                }
                .into(),
                avcc: vec![0, 0, 0, 2, nal, 1],
                ..Default::default()
            }),
        )),
        ..Default::default()
    }
}
#[test]
fn header_and_zero_first_offset_keep_original_native_contract() {
    let mut stream = VideoStream::new(lease());
    let VideoFrame::Header(header) = stream.accept(header(), Instant::now()).unwrap() else {
        panic!("header")
    };
    assert_eq!(header.dimensions.width(), 320);
    assert_eq!(header.rate.get(), 30);
    let VideoFrame::Sample(sample) = stream.accept(sample(2, 0, 0x65), Instant::now()).unwrap()
    else {
        panic!("sample")
    };
    assert_eq!(sample.presentation_micros(), 0);
    assert_eq!(sample.kind(), FrameKind::Keyframe);
    assert!(matches!(
        stream.accept(self::sample(3, 33_333, 0x41), Instant::now()),
        Ok(VideoFrame::Sample(_))
    ));
}
#[test]
fn unknown_codec_negative_dimensions_and_changed_lease_are_rejected_before_decoder() {
    for mode in 0..4 {
        let mut raw = header();
        let Some(wire::stream_media_session_response::Frame::Header(value)) = &mut raw.frame else {
            panic!("header")
        };
        match mode {
            0 => value.codec = buffa::EnumValue::Unknown(999),
            1 => value.width = -1,
            2 => {
                value.media_session.get_or_insert_default().expire_time = Timestamp {
                    seconds: 159,
                    ..Default::default()
                }
                .into()
            }
            3 => value.sps = vec![0x67, 100, 0, 30],
            _ => unreachable!(),
        };
        let mut stream = VideoStream::new(lease());
        assert!(stream.accept(raw, Instant::now()).is_err());
        assert!(
            stream.accept(header(), Instant::now()).is_err(),
            "a malformed stream cannot restart"
        );
    }
}
#[test]
fn sequence_first_keyframe_session_and_inband_parameter_changes_are_strict() {
    assert!(
        VideoStream::new(lease())
            .accept(sample(1, 0, 0x65), Instant::now())
            .is_err()
    );
    for mode in 0..5 {
        let mut stream = VideoStream::new(lease());
        stream.accept(header(), Instant::now()).unwrap();
        let mut raw = sample(2, 0, 0x65);
        match mode {
            0 => raw.sequence = 3,
            1 => {
                let Some(wire::stream_media_session_response::Frame::Sample(value)) =
                    &mut raw.frame
                else {
                    panic!("sample")
                };
                value.media_session =
                    format!("homes/synthetic/mediaSessions/s-{}q", "a".repeat(51));
            }
            2 => {
                let Some(wire::stream_media_session_response::Frame::Sample(value)) =
                    &mut raw.frame
                else {
                    panic!("sample")
                };
                value.avcc = vec![0, 0, 0, 2, 0x41, 1];
            }
            3 => {
                let Some(wire::stream_media_session_response::Frame::Sample(value)) =
                    &mut raw.frame
                else {
                    panic!("sample")
                };
                value.avcc = vec![0, 0, 0, 2, 0x67, 1];
            }
            4 => raw = header(),
            _ => unreachable!(),
        };
        assert!(stream.accept(raw, Instant::now()).is_err());
    }
}
#[test]
fn offsets_are_integral_microseconds_nonnegative_strictly_increasing_and_bounded() {
    for mode in 0..4 {
        let mut stream = VideoStream::new(lease());
        stream.accept(header(), Instant::now()).unwrap();
        stream.accept(sample(2, 0, 0x65), Instant::now()).unwrap();
        let mut raw = sample(3, 1, 0x41);
        let Some(wire::stream_media_session_response::Frame::Sample(value)) = &mut raw.frame else {
            panic!("sample")
        };
        match mode {
            0 => {
                value.presentation_duration = WireDuration {
                    nanos: 1,
                    ..Default::default()
                }
                .into()
            }
            1 => {
                value.presentation_duration = WireDuration {
                    seconds: -1,
                    ..Default::default()
                }
                .into()
            }
            2 => {
                value.presentation_duration = WireDuration {
                    seconds: 60,
                    nanos: 1000,
                    ..Default::default()
                }
                .into()
            }
            3 => value.presentation_duration = WireDuration::default().into(),
            _ => unreachable!(),
        };
        assert!(stream.accept(raw, Instant::now()).is_err());
    }
}
#[test]
fn lease_expiry_ends_admission_without_any_renewal_or_png_fallback() {
    let lease = lease();
    let deadline = lease.deadline();
    let mut stream = VideoStream::new(lease);
    assert!(stream.accept(header(), deadline).is_err());
    assert!(stream.accept(header(), Instant::now()).is_err());
}

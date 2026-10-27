use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    camera_video_contract::{VideoFrame, VideoStream},
    cameras_contract as camera,
    home::cameras::v1 as wire,
};
use std::time::{Duration, Instant, UNIX_EPOCH};

fn stamp(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
fn reference() -> wire::SessionRef {
    wire::SessionRef {
        entity_id: "driveway".into(),
        reference: vec![42; 32],
        ..Default::default()
    }
}
fn raw_lease() -> wire::Lease {
    wire::Lease {
        session: reference().into(),
        issued_at: stamp(100).into(),
        expires_at: stamp(130).into(),
        event_revision: 1,
        ..Default::default()
    }
}
fn lease(now: Instant) -> camera::Lease {
    let raw = wire::OpenSessionResponse {
        result: Some(wire::open_session_response::Result::Lease(Box::new(
            raw_lease(),
        ))),
        ..Default::default()
    };
    let camera::Outcome::Value(value) = camera::lease(
        &raw,
        &"driveway".parse().unwrap(),
        UNIX_EPOCH + Duration::from_secs(100),
        now,
    )
    .unwrap() else {
        panic!("lease")
    };
    value
}
fn header() -> wire::VideoHeader {
    wire::VideoHeader {
        lease: raw_lease().into(),
        codec: wire::VideoCodec::H264Baseline.into(),
        sps: vec![0x67, 66, 0, 31],
        pps: vec![0x68, 0],
        width: 320,
        height: 180,
        frames_per_second: 10,
        ..Default::default()
    }
}
fn response(sequence: u64, frame: wire::read_video_response::Frame) -> wire::ReadVideoResponse {
    wire::ReadVideoResponse {
        sequence,
        frame: Some(frame),
        ..Default::default()
    }
}
fn start(header: wire::VideoHeader) -> wire::ReadVideoResponse {
    response(
        1,
        wire::read_video_response::Frame::Header(Box::new(header)),
    )
}
fn sample(sequence: u64, stamp: u64, nal: u8) -> wire::ReadVideoResponse {
    response(
        sequence,
        wire::read_video_response::Frame::Sample(Box::new(wire::VideoSample {
            session: reference().into(),
            presentation_micros: stamp,
            avcc: vec![0, 0, 0, 2, nal, 0],
            ..Default::default()
        })),
    )
}
fn unknown<T: Message + Default>(value: T) -> T {
    let mut raw = value.encode_to_vec();
    raw.extend([0xf8, 0x07, 1]);
    T::decode_from_slice(&raw).unwrap()
}

#[test]
fn video_keeps_original_reference_monotonic_deadline_and_exact_frames() {
    let now = Instant::now();
    let mut stream = VideoStream::new(lease(now));
    let VideoFrame::Header(value) = stream.accept(start(header()), now).unwrap() else {
        panic!("header")
    };
    assert_eq!(value.dimensions.width(), 320);
    assert_eq!(value.rate.get(), 10);
    assert!(matches!(
        stream.accept(sample(2, 0, 0x65), now),
        Ok(VideoFrame::Sample(_))
    ));
    assert!(matches!(
        stream.accept(sample(3, 100_000, 0x41), now),
        Ok(VideoFrame::Sample(_))
    ));
    assert_eq!(
        stream
            .accept(sample(4, 200_000, 0x41), now + Duration::from_secs(30))
            .err(),
        Some(camera::AdmissionError::Expired)
    );
    assert!(stream.accept(sample(5, 300_000, 0x65), now).is_err());
}

#[test]
fn header_rejects_codec_capacity_unknown_fields_and_extended_or_foreign_lease() {
    let now = Instant::now();
    let mut cases = Vec::new();
    let mut value = header();
    value.codec = EnumValue::Unknown(55);
    cases.push(value);
    let mut value = header();
    value.frames_per_second = 0;
    cases.push(value);
    let mut value = header();
    value.frames_per_second = 31;
    cases.push(value);
    let mut value = header();
    value.width = 1921;
    cases.push(value);
    let mut value = header();
    value.height = 0;
    cases.push(value);
    let mut value = header();
    value.sps = vec![0; 1025];
    cases.push(value);
    let mut value = header();
    value.lease.as_option_mut().unwrap().expires_at = stamp(131).into();
    cases.push(value);
    let mut value = header();
    value
        .lease
        .as_option_mut()
        .unwrap()
        .session
        .as_option_mut()
        .unwrap()
        .entity_id = "_driveway".into();
    cases.push(value);
    let mut value = header();
    value
        .lease
        .as_option_mut()
        .unwrap()
        .session
        .as_option_mut()
        .unwrap()
        .reference[0] ^= 1;
    cases.push(value);
    let mut value = header();
    value.lease = unknown(raw_lease()).into();
    cases.push(value);
    let mut value = header();
    value.lease.as_option_mut().unwrap().issued_at = unknown(stamp(100)).into();
    cases.push(value);
    cases.push(unknown(header()));
    for value in cases {
        let mut stream = VideoStream::new(lease(now));
        assert!(stream.accept(start(value), now).is_err());
        assert!(stream.accept(start(header()), now).is_err());
    }
}

#[test]
fn video_requires_header_keyframe_identity_sequence_and_bounded_sample_order() {
    let now = Instant::now();
    assert!(
        VideoStream::new(lease(now))
            .accept(sample(1, 0, 0x65), now)
            .is_err()
    );
    for frame in [
        sample(2, 0, 0x41),
        sample(3, 0, 0x65),
        unknown(sample(2, 0, 0x65)),
    ] {
        let mut stream = VideoStream::new(lease(now));
        stream.accept(start(header()), now).unwrap();
        assert!(stream.accept(frame, now).is_err());
    }
    let mut frames = vec![
        sample(3, 0, 0x41),
        sample(3, 99_999, 0x41),
        sample(3, 60_000_001, 0x41),
        start(header()),
    ];
    let mut foreign = wire::VideoSample {
        session: reference().into(),
        presentation_micros: 100_000,
        avcc: vec![0, 0, 0, 2, 0x41, 0],
        ..Default::default()
    };
    foreign.session.as_option_mut().unwrap().entity_id = "_driveway".into();
    frames.push(response(
        3,
        wire::read_video_response::Frame::Sample(Box::new(foreign)),
    ));
    let mut renegotiated = sample(3, 100_000, 0x67);
    if let Some(wire::read_video_response::Frame::Sample(value)) = &mut renegotiated.frame {
        value.avcc = vec![0, 0, 0, 4, 0x67, 66, 0, 31];
    }
    frames.push(renegotiated);
    for frame in frames {
        let mut stream = VideoStream::new(lease(now));
        stream.accept(start(header()), now).unwrap();
        stream.accept(sample(2, 0, 0x65), now).unwrap();
        assert!(stream.accept(frame, now).is_err());
        assert!(stream.accept(sample(3, 100_000, 0x65), now).is_err());
    }
}

#[test]
fn terminal_fallbacks_never_admit_more_pixels() {
    use wire::read_video_response::Frame;
    let now = Instant::now();
    for frame in [
        Frame::Unsupported(Box::default()),
        Frame::Unavailable(Box::default()),
        Frame::Revoked(Box::default()),
        Frame::Expired(Box::default()),
        Frame::ResyncRequired(Box::default()),
    ] {
        let mut stream = VideoStream::new(lease(now));
        stream.accept(response(1, frame), now).unwrap();
        assert!(stream.accept(start(header()), now).is_err());
    }
}

//! Actual native-session TLS -> stateful video admission -> owned blocking
//! VideoToolbox worker. No Slint rendering or Android qualification is claimed.
#[cfg(target_os = "macos")]
mod probe {
    use home_client_model::{cameras::CameraId, video::VideoDimensions};
    use home_client_platform::video::{DecoderBackend, H264Decoder};
    use home_client_transport::{AccessToken, CameraTransport, Connection};
    use home_wire_contract::{
        camera_video_contract::{VideoFrame, VideoStream},
        cameras_contract as camera,
        home::{auth::v1 as auth, cameras::v1 as wire},
    };
    use serde::{Deserialize, Serialize};
    use sha2::{Digest, Sha256};
    use std::{
        collections::BTreeSet,
        path::{Path, PathBuf},
        time::{Duration, Instant, SystemTime},
    };
    use tokio_util::sync::CancellationToken;

    #[derive(Deserialize)]
    #[serde(rename_all = "kebab-case")]
    enum Mode {
        SyntheticOnly,
    }
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Credential {
        login_name: String,
        password: String,
    }
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Fixture {
        mode: Mode,
        origin: String,
        ca_pem: String,
        reader: Credential,
        production_cameras: bool,
    }
    #[derive(Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
    #[serde(rename_all = "kebab-case")]
    enum Control {
        Snapshot,
    }
    #[derive(PartialEq, Eq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Instruction {
        sequence: u64,
        action: Control,
    }
    enum Worker {
        AwaitingHeader,
        Decoding {
            decoder: H264Decoder,
            dimensions: VideoDimensions,
        },
    }

    fn decode(
        mut input: tokio::sync::mpsc::Receiver<VideoFrame>,
    ) -> Result<serde_json::Value, String> {
        let mut worker = Worker::AwaitingHeader;
        let mut count = 0;
        let mut hashes = BTreeSet::new();
        while let Some(frame) = input.blocking_recv() {
            match frame {
                VideoFrame::Header(header) => {
                    match worker {
                        Worker::AwaitingHeader => {}
                        Worker::Decoding { .. } => return Err("duplicate video header".into()),
                    }
                    let decoder = H264Decoder::new(&header.format)
                        .map_err(|error| format!("native decoder: {error:?}"))?;
                    match decoder.backend() {
                        DecoderBackend::VideoToolboxHardware => {}
                    }
                    if decoder.dimensions()
                        != (header.dimensions.width(), header.dimensions.height())
                    {
                        return Err("native format dimensions differ".into());
                    }
                    worker = Worker::Decoding {
                        decoder,
                        dimensions: header.dimensions,
                    };
                }
                VideoFrame::Sample(sample) => {
                    let (decoder, dimensions) = match &mut worker {
                        Worker::AwaitingHeader => return Err("sample without header".into()),
                        Worker::Decoding {
                            decoder,
                            dimensions,
                        } => (decoder, dimensions),
                    };
                    let frame = decoder
                        .decode(&sample)
                        .map_err(|error| format!("native decode: {error:?}"))?;
                    if frame.dimensions() != (dimensions.width(), dimensions.height()) {
                        return Err("native output dimensions differ".into());
                    }
                    hashes.insert(format!("{:x}", Sha256::digest(frame.pixels())));
                    count += 1;
                }
                VideoFrame::Expired
                | VideoFrame::Revoked
                | VideoFrame::Unavailable
                | VideoFrame::Unsupported
                | VideoFrame::ResyncRequired => {
                    return Err("unexpected positive-stream terminal".into());
                }
            }
        }
        if count != 6 || hashes.len() != 6 {
            return Err("six actual moving decoded frames required".into());
        }
        // Decoder is dropped here on the SAME blocking thread before join returns.
        Ok(
            serde_json::json!({"decoded_frames":count,"distinct_decoded_frames":hashes.len(),"hardware":true,"decoder_joined":true}),
        )
    }

    async fn open(
        api: &CameraTransport,
        id: &CameraId,
        token: &AccessToken,
        cancel: &CancellationToken,
    ) -> camera::Lease {
        let response = api
            .open_session(
                wire::OpenSessionRequest {
                    entity_id: id.entity().as_str().into(),
                    ..Default::default()
                },
                token,
                Duration::from_secs(5),
                cancel,
            )
            .await
            .expect("open synthetic camera lease");
        let camera::Outcome::Value(lease) =
            camera::lease(&response, id, SystemTime::now(), Instant::now())
                .expect("camera lease admission")
        else {
            panic!("synthetic camera unavailable")
        };
        lease
    }
    async fn play(api: &CameraTransport, id: &CameraId, token: &AccessToken) -> serde_json::Value {
        let cancel = CancellationToken::new();
        let lease = open(api, id, token, &cancel).await;
        let mut admission = VideoStream::new(lease.clone());
        let mut stream = api
            .read_video(
                wire::ReadVideoRequest {
                    session: lease.wire_reference().into(),
                    ..Default::default()
                },
                token,
                Duration::from_secs(10),
                &cancel,
            )
            .await
            .expect("native TLS video stream");
        let (send, receive) = tokio::sync::mpsc::channel(1);
        let worker = tokio::task::spawn_blocking(move || decode(receive));
        let mut samples = 0;
        while samples < 6 {
            let raw = stream
                .next()
                .await
                .expect("video transport")
                .expect("early video EOF");
            let frame = admission
                .accept(raw, Instant::now())
                .expect("video identity/order/deadline admission");
            match &frame {
                VideoFrame::Sample(_) => samples += 1,
                VideoFrame::Header(_) => {}
                VideoFrame::Expired
                | VideoFrame::Revoked
                | VideoFrame::Unavailable
                | VideoFrame::Unsupported
                | VideoFrame::ResyncRequired => panic!("unexpected video refusal"),
            }
            assert!(send.send(frame).await.is_ok(), "decoder stopped");
        }
        cancel.cancel();
        drop(stream);
        drop(send);
        let result = worker
            .await
            .expect("native worker join")
            .expect("native actual moving frames");
        // Cleanup uses a separate live cancellation token. Never reuse a read token
        // that was cancelled to stop the body, and never retry a write.
        api.close_session(
            wire::CloseSessionRequest {
                session: lease.wire_reference().into(),
                ..Default::default()
            },
            token,
            Duration::from_secs(5),
            &CancellationToken::new(),
        )
        .await
        .expect("close native camera lease");
        result
    }
    async fn joined(root: &Path, sequence: &mut u64) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                *sequence += 1;
                assert!(*sequence <= 128);
                let command = Instruction {
                    sequence: *sequence,
                    action: Control::Snapshot,
                };
                let temporary = root.join("control.writing");
                std::fs::write(&temporary, serde_json::to_vec(&command).unwrap()).unwrap();
                std::fs::rename(temporary, root.join("control.json")).unwrap();
                let ack = root.join(format!("ack-{:02}.json", *sequence));
                while !ack.is_file() {
                    tokio::time::sleep(Duration::from_millis(10)).await;
                }
                let actual: Instruction =
                    serde_json::from_slice(&std::fs::read(ack).unwrap()).unwrap();
                assert!(actual == command);
                let stats: serde_json::Value = serde_json::from_slice(
                    &std::fs::read(root.join(format!("stats-{:02}.json", *sequence))).unwrap(),
                )
                .unwrap();
                if stats["video_streams"].as_i64() == Some(0) {
                    break;
                }
            }
        })
        .await
        .expect("server video must join");
    }
    pub async fn run() {
        let root = PathBuf::from(
            std::env::args_os()
                .nth(1)
                .expect("absolute isolated fixture directory required"),
        );
        assert!(root.is_absolute());
        let fixture: Fixture =
            serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
        match fixture.mode {
            Mode::SyntheticOnly => {}
        };
        assert!(!fixture.production_cameras);
        assert!(
            fixture.origin.starts_with("https://127.0.0.1:"),
            "isolated loopback TLS fixture only"
        );
        let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())
            .expect("verified fixture TLS");
        let cancel = CancellationToken::new();
        let login = connection
            .auth()
            .start_login(
                auth::StartLoginRequest {
                    login_name: fixture.reader.login_name,
                    password: fixture.reader.password,
                    ..Default::default()
                },
                Duration::from_secs(5),
                &cancel,
            )
            .await
            .expect("native session login");
        let Some(auth::login_decision::Outcome::Authenticated(tokens)) =
            login.decision.outcome.as_ref()
        else {
            panic!("native authentication required")
        };
        let token = AccessToken::parse(&tokens.access_token).unwrap();
        let api = connection.cameras();
        let raw = api
            .list_cameras(
                wire::ListCamerasRequest::default(),
                &token,
                Duration::from_secs(5),
                &cancel,
            )
            .await
            .unwrap();
        let cameras = camera::catalog(&raw).unwrap();
        assert_eq!(cameras.len(), 2);
        assert_ne!(cameras[0].id, cameras[1].id);
        let mut results = Vec::new();
        let mut sequence = 0;
        for camera in cameras {
            results.push(play(&api, &camera.id, &token).await);
            joined(&root, &mut sequence).await;
        }
        println!(
            "{}",
            serde_json::json!({"status":"PASS","actual_rust_go_tls":true,"native_hardware_decode":true,"native_accounts_and_exact_camera_leases":true,"sources":results,"server_streams_joined":true,"bounded_worker_queue":1,"native_slint_rendering":false,"resource_growth_measured":false,"android_qualified":false,"production_video":false})
        );
    }
}

#[cfg(target_os = "macos")]
#[tokio::main]
async fn main() {
    probe::run().await
}

#[cfg(not(target_os = "macos"))]
fn main() -> Result<(), Box<dyn std::error::Error>> {
    Err("requires actual macOS hardware decoder".into())
}

//! Explicit macOS codec probe on an original synthetic Annex-B fixture.
//! This proves the native codec only, not network authorization or Slint rendering.
#[cfg(target_os = "macos")]
fn main() -> Result<(), Box<dyn std::error::Error>> {
    use home_client_platform::video::{DecoderBackend, EncodedFrame, H264Decoder, H264Format};
    use sha2::{Digest, Sha256};
    use std::{collections::BTreeSet, io::Read, time::Instant};

    let path = std::env::args_os()
        .nth(1)
        .ok_or("synthetic Annex-B file required")?;
    let mut source = Vec::new();
    std::fs::File::open(path)?
        .take(16 * 1024 * 1024 + 1)
        .read_to_end(&mut source)?;
    if source.len() > 16 * 1024 * 1024 {
        return Err("fixture too large".into());
    }
    let mut starts = Vec::new();
    let mut offset = 0;
    while offset + 3 <= source.len() {
        let length = if source[offset..].starts_with(&[0, 0, 0, 1]) {
            4
        } else if source[offset..].starts_with(&[0, 0, 1]) {
            3
        } else {
            offset += 1;
            continue;
        };
        starts.push((offset, offset + length));
        offset += length;
    }
    let mut sps = None;
    let mut pps = None;
    let mut samples: Vec<Vec<u8>> = Vec::new();
    for (index, (_, payload)) in starts.iter().enumerate() {
        let end = starts
            .get(index + 1)
            .map_or(source.len(), |(start, _)| *start);
        let nal = &source[*payload..end];
        let header = *nal.first().ok_or("empty fixture NAL")?;
        match header & 31 {
            7 => {
                if sps.as_ref().is_some_and(|old: &Vec<u8>| old != nal) {
                    return Err("fixture SPS changes".into());
                }
                sps = Some(nal.to_vec());
            }
            8 => {
                if pps.as_ref().is_some_and(|old: &Vec<u8>| old != nal) {
                    return Err("fixture PPS changes".into());
                }
                pps = Some(nal.to_vec());
            }
            9 => {
                if samples.len() >= 120 {
                    return Err("too many fixture frames".into());
                }
                samples.push(Vec::new());
            }
            1 | 5 | 6 => {}
            _ => return Err("unexpected fixture NAL".into()), // Open H.264 NAL codes.
        }
        if matches!(header & 31, 1 | 5 | 6 | 9) {
            let sample = samples.last_mut().ok_or("fixture must contain AUDs")?;
            sample.extend_from_slice(&(nal.len() as u32).to_be_bytes());
            sample.extend_from_slice(nal);
        }
    }
    let format = H264Format::new(sps.ok_or("SPS missing")?, pps.ok_or("PPS missing")?)
        .map_err(|error| format!("format: {error:?}"))?;
    let frames: Vec<EncodedFrame> = samples
        .into_iter()
        .enumerate()
        .map(|(index, bytes)| EncodedFrame::new(bytes, index as u64 * 100_000))
        .collect::<Result<_, _>>()
        .map_err(|error| format!("admission: {error:?}"))?;
    if frames.len() < 10 {
        return Err("at least ten actual frames required".into());
    }
    let started = Instant::now();
    let mut hashes = BTreeSet::new();
    let mut dimensions = None;
    let mut decoded = 0;
    for _cycle in 0..100 {
        let mut decoder =
            H264Decoder::new(&format).map_err(|error| format!("create: {error:?}"))?;
        match decoder.backend() {
            DecoderBackend::VideoToolboxHardware => {}
        }
        dimensions = Some(decoder.dimensions());
        for frame in &frames {
            let pixels = decoder
                .decode(frame)
                .map_err(|error| format!("decode: {error:?}"))?;
            if pixels.dimensions() != decoder.dimensions() {
                return Err("output dimensions changed".into());
            }
            hashes.insert(format!("{:x}", Sha256::digest(pixels.pixels())));
            decoded += 1;
        }
        // Actual Drop invalidates/releases the native session on every cycle.
    }
    if hashes.len() < 10 {
        return Err("decoded output did not move".into());
    }
    println!(
        "{}",
        serde_json::json!({
            "status": "PASS", "backend": "VideoToolboxHardware", "hardware_required": true,
            "codec": "H264Baseline", "audio": false, "cycles": 100,
            "frames_per_cycle": frames.len(), "decoded_frames": decoded,
            "distinct_decoded_frames": hashes.len(), "dimensions": dimensions,
            "elapsed_millis": started.elapsed().as_millis(),
            "scope": "native codec only", "network_qualified": false, "slint_qualified": false,
            "android_qualified": false, "persistent_resource_growth_measured": false
        })
    );
    Ok(())
}

#[cfg(not(target_os = "macos"))]
fn main() -> Result<(), Box<dyn std::error::Error>> {
    Err("this probe requires actual macOS VideoToolbox hardware".into())
}

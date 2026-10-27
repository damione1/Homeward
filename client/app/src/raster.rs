//! Bounded in-memory raster decoding. Call on an owned background task.
use std::{io::Cursor, sync::Arc};

#[derive(Clone, Copy)]
pub(crate) struct Limits {
    pub encoded: usize,
    pub side: u32,
    pub decoded: usize,
    pub decoder: usize,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Failure {
    Invalid,
    Capacity,
}
#[derive(Clone)]
pub(crate) struct Pixels {
    pub width: u32,
    pub height: u32,
    pub rgba: Arc<[u8]>,
}

pub(crate) fn decode_png(bytes: &[u8], limits: Limits) -> Result<Pixels, Failure> {
    if bytes.len() > limits.encoded {
        return Err(Failure::Capacity);
    }
    let mut decoder = png::Decoder::new(Cursor::new(bytes));
    decoder.set_limits(png::Limits {
        bytes: limits.decoder,
    });
    decoder.set_transformations(png::Transformations::EXPAND | png::Transformations::STRIP_16);
    let mut reader = decoder.read_info().map_err(|_| Failure::Invalid)?;
    let info = reader.info();
    if info.width == 0
        || info.height == 0
        || info.width > limits.side
        || info.height > limits.side
        || info.animation_control.is_some()
    {
        return Err(Failure::Capacity);
    }
    let required = (info.width as usize)
        .checked_mul(info.height as usize)
        .and_then(|n| n.checked_mul(4))
        .filter(|n| *n <= limits.decoded)
        .ok_or(Failure::Capacity)?;
    let size = reader
        .output_buffer_size()
        .filter(|n| *n <= limits.decoded)
        .ok_or(Failure::Capacity)?;
    let mut buffer = vec![0; size];
    let output = reader
        .next_frame(&mut buffer)
        .map_err(|_| Failure::Invalid)?;
    let mut rgba = Vec::with_capacity(required);
    let data = &buffer[..output.buffer_size()];
    match output.color_type {
        png::ColorType::Rgba => rgba.extend_from_slice(data),
        png::ColorType::Rgb => {
            for p in data.as_chunks::<3>().0 {
                rgba.extend_from_slice(&[p[0], p[1], p[2], 255]);
            }
        }
        png::ColorType::Grayscale => {
            for p in data {
                rgba.extend_from_slice(&[*p, *p, *p, 255]);
            }
        }
        png::ColorType::GrayscaleAlpha => {
            for p in data.as_chunks::<2>().0 {
                rgba.extend_from_slice(&[p[0], p[0], p[0], p[1]]);
            }
        }
        png::ColorType::Indexed => return Err(Failure::Invalid),
    }
    reader.finish().map_err(|_| Failure::Invalid)?;
    if rgba.len() != required {
        return Err(Failure::Invalid);
    }
    Ok(Pixels {
        width: output.width,
        height: output.height,
        rgba: rgba.into(),
    })
}

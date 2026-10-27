//! Single-owner synchronous VideoToolbox adapter, called off the UI thread.
//! Hardware is required at creation AND checked via the actual session property.
//! No async/temporal flags: Apple guarantees callback completion before DecodeFrame
//! returns. The callback copies bounded RGBA; no CVPixelBuffer escapes its scope.
//! https://developer.apple.com/documentation/videotoolbox/vtdecompressionsessiondecodeframe(_:samplebuffer:flags:framerefcon:infoflagsout:)
mod ffi;
use super::{
    DecodeFailure, DecodedFrame, DecoderBackend, EncodedFrame, FrameKind, H264Format, InputFailure,
    MAX_HEIGHT, MAX_WIDTH,
};
use core_foundation_sys::{
    base::{CFGetTypeID, CFRelease, CFTypeRef},
    dictionary::{
        CFDictionaryCreate, kCFTypeDictionaryKeyCallBacks, kCFTypeDictionaryValueCallBacks,
    },
    number::{
        CFBooleanGetTypeID, CFBooleanGetValue, CFNumberCreate, kCFBooleanTrue, kCFNumberSInt32Type,
    },
};
use std::{
    ffi::c_void,
    marker::PhantomData,
    ptr::{NonNull, null, null_mut},
    rc::Rc,
    sync::Mutex,
};
use zeroize::Zeroizing;

const BGRA: u32 = u32::from_be_bytes(*b"BGRA");
const READ_ONLY: u64 = 1;
const INVALID_TIME: ffi::Time = ffi::Time {
    value: 0,
    scale: 0,
    flags: 0,
    epoch: 0,
};

struct OwnedCf(NonNull<c_void>);
impl OwnedCf {
    // Own one Create/Copy reference, including cleanup of partial error outputs.
    fn created(status: i32, pointer: CFTypeRef) -> Result<Self, DecodeFailure> {
        let owned = NonNull::new(pointer.cast_mut()).map(Self);
        if status != 0 {
            return Err(DecodeFailure::Native(status));
        }
        owned.ok_or(DecodeFailure::MissingOutput)
    }
    fn raw(&self) -> CFTypeRef {
        self.0.as_ptr()
    }
}
impl Drop for OwnedCf {
    fn drop(&mut self) {
        // SAFETY: exactly one owned, non-null CF reference.
        unsafe { CFRelease(self.raw()) };
    }
}

fn status(code: i32) -> Result<(), DecodeFailure> {
    if code == 0 {
        Ok(())
    } else {
        Err(DecodeFailure::Native(code))
    }
}
fn dictionary(key: CFTypeRef, value: CFTypeRef) -> Result<OwnedCf, DecodeFailure> {
    // SAFETY: caller supplies live CF values; dictionary retains both.
    OwnedCf::created(0, unsafe {
        CFDictionaryCreate(
            null(),
            &key,
            &value,
            1,
            &kCFTypeDictionaryKeyCallBacks,
            &kCFTypeDictionaryValueCallBacks,
        )
        .cast()
    })
}

#[derive(Clone, Copy)]
enum ReferenceState {
    AwaitingKeyframe,
    Ready,
}

/// Intentionally !Send + !Sync. Create/use/drop on one owned blocking worker.
/// Drop invalidates the decoder before releasing the session and format.
pub struct H264Decoder {
    session: OwnedCf,
    format: OwnedCf,
    width: u32,
    height: u32,
    reference: ReferenceState,
    last_timestamp: Option<i64>,
    _thread: PhantomData<Rc<()>>,
}
impl H264Decoder {
    pub fn new(format: &H264Format) -> Result<Self, DecodeFailure> {
        let pointers = [format.sps().as_ptr(), format.pps().as_ptr()];
        let sizes = [format.sps().len(), format.pps().len()];
        let mut raw = null();
        // SAFETY: two bounded live parameter NALs and valid out pointer. CM copies them.
        let native_format = OwnedCf::created(
            unsafe {
                ffi::CMVideoFormatDescriptionCreateFromH264ParameterSets(
                    null(),
                    2,
                    pointers.as_ptr(),
                    sizes.as_ptr(),
                    4,
                    &mut raw,
                )
            },
            raw,
        )?;
        // SAFETY: format was created successfully as a video format description.
        let dimensions = unsafe { ffi::CMVideoFormatDescriptionGetDimensions(native_format.raw()) };
        let width = u32::try_from(dimensions.width).map_err(|_| DecodeFailure::DimensionLimit)?;
        let height = u32::try_from(dimensions.height).map_err(|_| DecodeFailure::DimensionLimit)?;
        if width == 0 || height == 0 || width > MAX_WIDTH || height > MAX_HEIGHT {
            return Err(DecodeFailure::DimensionLimit);
        }
        // SAFETY: exported framework constants are process-lifetime CF values.
        let specification = unsafe {
            dictionary(
                ffi::kVTVideoDecoderSpecification_RequireHardwareAcceleratedVideoDecoder.cast(),
                kCFBooleanTrue.cast(),
            )
        }?;
        let pixel_format = BGRA as i32;
        // SAFETY: CFNumber copies the passed i32.
        let number = OwnedCf::created(0, unsafe {
            CFNumberCreate(
                null(),
                kCFNumberSInt32Type,
                (&pixel_format as *const i32).cast(),
            )
            .cast()
        })?;
        // SAFETY: exported constant plus live number, retained by dictionary.
        let attributes =
            unsafe { dictionary(ffi::kCVPixelBufferPixelFormatTypeKey.cast(), number.raw()) }?;
        let callback = ffi::Callback {
            function: output_callback,
            context: null_mut(),
        };
        let mut raw_session = null();
        // SAFETY: all input CF objects live through creation; callback has static code
        // and a per-frame scoped context only supplied during synchronous decode.
        let session = OwnedCf::created(
            unsafe {
                ffi::VTDecompressionSessionCreate(
                    null(),
                    native_format.raw(),
                    specification.raw(),
                    attributes.raw(),
                    &callback,
                    &mut raw_session,
                )
            },
            raw_session,
        )?;
        let decoder = Self {
            session,
            format: native_format,
            width,
            height,
            reference: ReferenceState::AwaitingKeyframe,
            last_timestamp: None,
            _thread: PhantomData,
        };
        let mut property = null();
        // SAFETY: live session, exported property key and out pointer. Copy gives ownership.
        let hardware = OwnedCf::created(
            unsafe {
                ffi::VTSessionCopyProperty(
                    decoder.session.raw(),
                    ffi::kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder,
                    null(),
                    &mut property,
                )
            },
            property,
        )?;
        // SAFETY: check CF type before calling the Boolean-specific API.
        if unsafe {
            CFGetTypeID(hardware.raw()) != CFBooleanGetTypeID()
                || !CFBooleanGetValue(hardware.raw().cast())
        } {
            return Err(DecodeFailure::HardwareUnavailable);
        }
        Ok(decoder)
    }
    pub fn backend(&self) -> DecoderBackend {
        DecoderBackend::VideoToolboxHardware
    }
    pub fn dimensions(&self) -> (u32, u32) {
        (self.width, self.height)
    }

    pub fn decode(&mut self, frame: &EncodedFrame) -> Result<DecodedFrame, DecodeFailure> {
        match self.reference {
            ReferenceState::AwaitingKeyframe => match frame.kind() {
                FrameKind::Keyframe => {}
                FrameKind::Predicted => return Err(DecodeFailure::KeyframeRequired),
            },
            ReferenceState::Ready => {}
        }
        if self
            .last_timestamp
            .is_some_and(|last| frame.presentation_micros() <= last)
        {
            return Err(DecodeFailure::Input(InputFailure::InvalidSample));
        }
        let result = self.decode_sample(frame);
        match &result {
            Ok(_) => {
                self.reference = ReferenceState::Ready;
                self.last_timestamp = Some(frame.presentation_micros());
            }
            Err(_) => {
                self.reference = ReferenceState::AwaitingKeyframe;
            }
        }
        result
    }

    fn decode_sample(&self, frame: &EncodedFrame) -> Result<DecodedFrame, DecodeFailure> {
        let length = frame.bytes().len();
        let mut raw_block = null();
        // SAFETY: null memory asks CoreMedia to allocate its own bounded buffer.
        let block = OwnedCf::created(
            unsafe {
                ffi::CMBlockBufferCreateWithMemoryBlock(
                    null(),
                    null_mut(),
                    length,
                    null(),
                    null(),
                    0,
                    length,
                    0,
                    &mut raw_block,
                )
            },
            raw_block,
        )?;
        // SAFETY: owned block of length bytes, and equally sized borrowed source.
        status(unsafe {
            ffi::CMBlockBufferReplaceDataBytes(
                frame.bytes().as_ptr().cast(),
                block.raw(),
                0,
                length,
            )
        })?;
        let timing = ffi::Timing {
            duration: INVALID_TIME,
            presentation: ffi::Time {
                value: frame.presentation_micros(),
                scale: 1_000_000,
                flags: 1,
                epoch: 0,
            },
            decode: INVALID_TIME,
        };
        let mut raw_sample = null();
        // SAFETY: one owned sample, one timestamp and exact size; CMSampleBuffer
        // retains the block/format until its own release.
        let sample = OwnedCf::created(
            unsafe {
                ffi::CMSampleBufferCreateReady(
                    null(),
                    block.raw(),
                    self.format.raw(),
                    1,
                    1,
                    &timing,
                    1,
                    &length,
                    &mut raw_sample,
                )
            },
            raw_sample,
        )?;
        let output = Output {
            width: self.width,
            height: self.height,
            result: Mutex::new(None),
        };
        // SAFETY: flags=0 disables async AND temporal processing. Apple guarantees
        // completion of callbacks before return. `output` lives through this call;
        // Mutex also makes a callback on an internal decoder thread safe.
        status(unsafe {
            ffi::VTDecompressionSessionDecodeFrame(
                self.session.raw(),
                sample.raw(),
                0,
                (&output as *const Output).cast_mut().cast(),
                null_mut(),
            )
        })?;
        output
            .result
            .into_inner()
            .map_err(|_| DecodeFailure::InvalidOutput)?
            .ok_or(DecodeFailure::MissingOutput)?
    }
}
impl Drop for H264Decoder {
    fn drop(&mut self) {
        // SAFETY: live owned session; no outstanding callbacks (synchronous only).
        unsafe { ffi::VTDecompressionSessionInvalidate(self.session.raw()) };
    }
}

struct Output {
    width: u32,
    height: u32,
    result: Mutex<Option<Result<DecodedFrame, DecodeFailure>>>,
}
unsafe extern "C" fn output_callback(
    _session: *mut c_void,
    context: *mut c_void,
    code: i32,
    _flags: u32,
    image: CFTypeRef,
    _time: ffi::Time,
    _duration: ffi::Time,
) {
    // SAFETY: set only by decode_sample; scoped until synchronous call returns.
    let Some(output) = (unsafe { context.cast::<Output>().as_ref() }) else {
        return;
    };
    let result = status(code).and_then(|()| copy_pixels(image, output.width, output.height));
    if let Ok(mut slot) = output.result.lock() {
        if slot.is_some() {
            *slot = Some(Err(DecodeFailure::InvalidOutput));
        } else {
            *slot = Some(result);
        }
    }
}

struct PixelLock(CFTypeRef);
impl Drop for PixelLock {
    fn drop(&mut self) {
        // SAFETY: paired with one successful read-only lock.
        unsafe { ffi::CVPixelBufferUnlockBaseAddress(self.0, READ_ONLY) };
    }
}
fn copy_pixels(image: CFTypeRef, width: u32, height: u32) -> Result<DecodedFrame, DecodeFailure> {
    if image.is_null() {
        return Err(DecodeFailure::MissingOutput);
    }
    // SAFETY: VideoToolbox supplies a live CVPixelBuffer throughout its callback.
    unsafe {
        if ffi::CVPixelBufferGetWidth(image) != width as usize
            || ffi::CVPixelBufferGetHeight(image) != height as usize
            || ffi::CVPixelBufferGetPixelFormatType(image) != BGRA
            || ffi::CVPixelBufferIsPlanar(image) != 0
        {
            return Err(DecodeFailure::InvalidOutput);
        }
        status(ffi::CVPixelBufferLockBaseAddress(image, READ_ONLY))?;
        let _lock = PixelLock(image);
        let row_bytes = width as usize * 4;
        let stride = ffi::CVPixelBufferGetBytesPerRow(image);
        let length = stride
            .checked_mul(height as usize)
            .ok_or(DecodeFailure::InvalidOutput)?;
        let size = ffi::CVPixelBufferGetDataSize(image);
        let base = ffi::CVPixelBufferGetBaseAddress(image).cast::<u8>();
        if stride < row_bytes
            || length > size
            || length > MAX_WIDTH as usize * MAX_HEIGHT as usize * 8
            || base.is_null()
        {
            return Err(DecodeFailure::InvalidOutput);
        }
        let source = std::slice::from_raw_parts(base, length);
        let mut pixels = Zeroizing::new(vec![0; row_bytes * height as usize]);
        for (input, output) in source
            .chunks_exact(stride)
            .zip(pixels.chunks_exact_mut(row_bytes))
        {
            for (bgra, rgba) in input[..row_bytes]
                .as_chunks::<4>()
                .0
                .iter()
                .zip(output.as_chunks_mut::<4>().0.iter_mut())
            {
                rgba.copy_from_slice(&[bgra[2], bgra[1], bgra[0], 255]);
            }
        }
        Ok(DecodedFrame {
            width,
            height,
            pixels,
        })
    }
}

//! Narrow SDK ABI declarations; see macOS CoreMedia/VideoToolbox headers.
#![allow(non_snake_case)]
use core_foundation_sys::{base::CFTypeRef, string::CFStringRef};
use std::ffi::c_void;

#[repr(C, packed(4))]
#[derive(Clone, Copy)]
pub(super) struct Time {
    pub value: i64,
    pub scale: i32,
    pub flags: u32,
    pub epoch: i64,
}
#[repr(C, packed(4))]
pub(super) struct Timing {
    pub duration: Time,
    pub presentation: Time,
    pub decode: Time,
}
#[repr(C, packed(4))]
pub(super) struct Dimensions {
    pub width: i32,
    pub height: i32,
}
#[repr(C, packed(4))]
pub(super) struct Callback {
    pub function: unsafe extern "C" fn(*mut c_void, *mut c_void, i32, u32, CFTypeRef, Time, Time),
    pub context: *mut c_void,
}

#[link(name = "VideoToolbox", kind = "framework")]
unsafe extern "C" {
    pub(super) static kVTVideoDecoderSpecification_RequireHardwareAcceleratedVideoDecoder:
        CFStringRef;
    pub(super) static kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder: CFStringRef;
    pub(super) fn VTDecompressionSessionCreate(
        allocator: CFTypeRef,
        format: CFTypeRef,
        specification: CFTypeRef,
        attributes: CFTypeRef,
        callback: *const Callback,
        session: *mut CFTypeRef,
    ) -> i32;
    pub(super) fn VTDecompressionSessionInvalidate(session: CFTypeRef);
    pub(super) fn VTDecompressionSessionDecodeFrame(
        session: CFTypeRef,
        sample: CFTypeRef,
        flags: u32,
        context: *mut c_void,
        output_flags: *mut u32,
    ) -> i32;
    pub(super) fn VTSessionCopyProperty(
        session: CFTypeRef,
        key: CFStringRef,
        allocator: CFTypeRef,
        value: *mut CFTypeRef,
    ) -> i32;
}

#[link(name = "CoreMedia", kind = "framework")]
unsafe extern "C" {
    pub(super) fn CMVideoFormatDescriptionCreateFromH264ParameterSets(
        allocator: CFTypeRef,
        count: usize,
        parameters: *const *const u8,
        sizes: *const usize,
        length_bytes: i32,
        format: *mut CFTypeRef,
    ) -> i32;
    pub(super) fn CMVideoFormatDescriptionGetDimensions(format: CFTypeRef) -> Dimensions;
    pub(super) fn CMBlockBufferCreateWithMemoryBlock(
        allocator: CFTypeRef,
        memory: *mut c_void,
        length: usize,
        block_allocator: CFTypeRef,
        source: CFTypeRef,
        offset: usize,
        data_length: usize,
        flags: u32,
        buffer: *mut CFTypeRef,
    ) -> i32;
    pub(super) fn CMBlockBufferReplaceDataBytes(
        data: *const c_void,
        buffer: CFTypeRef,
        offset: usize,
        length: usize,
    ) -> i32;
    pub(super) fn CMSampleBufferCreateReady(
        allocator: CFTypeRef,
        data: CFTypeRef,
        format: CFTypeRef,
        samples: isize,
        timing_count: isize,
        timing: *const Timing,
        size_count: isize,
        size: *const usize,
        sample: *mut CFTypeRef,
    ) -> i32;
}

#[link(name = "CoreVideo", kind = "framework")]
unsafe extern "C" {
    pub(super) static kCVPixelBufferPixelFormatTypeKey: CFStringRef;
    pub(super) fn CVPixelBufferGetWidth(pixel: CFTypeRef) -> usize;
    pub(super) fn CVPixelBufferGetHeight(pixel: CFTypeRef) -> usize;
    pub(super) fn CVPixelBufferGetBytesPerRow(pixel: CFTypeRef) -> usize;
    pub(super) fn CVPixelBufferGetDataSize(pixel: CFTypeRef) -> usize;
    pub(super) fn CVPixelBufferGetPixelFormatType(pixel: CFTypeRef) -> u32;
    pub(super) fn CVPixelBufferIsPlanar(pixel: CFTypeRef) -> u8;
    pub(super) fn CVPixelBufferGetBaseAddress(pixel: CFTypeRef) -> *mut c_void;
    pub(super) fn CVPixelBufferLockBaseAddress(pixel: CFTypeRef, flags: u64) -> i32;
    pub(super) fn CVPixelBufferUnlockBaseAddress(pixel: CFTypeRef, flags: u64) -> i32;
}

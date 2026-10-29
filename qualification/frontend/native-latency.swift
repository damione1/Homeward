import AppKit
import ApplicationServices
import CoreMedia
import CoreVideo
import ImageIO
import ScreenCaptureKit
import UniformTypeIdentifiers

// Qualification only. Capture one already identified synthetic Homeward window,
// retain only its heading, and alternate two local navigation actions. No app
// callback, timer gap or AX response is substituted for compositor display time.
enum ProbeFailure: Error {
    case invalidInput, permissions, missingWindow, geometry, action, heading
    case capture, frameTimeout, frameClock, templates, transitionTimeout, stream
}
enum MeasurementStatus: String { case measured = "MEASURED", overflow = "CAPTURE_OVERFLOW_NO_VERDICT" }
enum CapturePhase { case resting, measuring }
enum Destination: String, Codable {
    case home, inventory
    var label: String {
        switch self { case .home: return "Accueil"; case .inventory: return "Inventaire" }
    }
    func isHeading(_ text: String) -> Bool {
        switch self {
        case .home: return text.hasPrefix("Ma maison ·")
        case .inventory: return text == "Inventaire"
        }
    }
    var opposite: Self { switch self { case .home: return .inventory; case .inventory: return .home } }
}
struct Input: Decodable { let pid: Int32; let syntheticOnly: Bool; let outputDirectory: String }
struct Clock: Codable {
    let numer: UInt32
    let denom: UInt32
    init() throws {
        var info = mach_timebase_info_data_t()
        guard mach_timebase_info(&info) == KERN_SUCCESS, info.numer > 0, info.denom > 0 else { throw ProbeFailure.frameClock }
        numer = info.numer; denom = info.denom
    }
    func nanoseconds(_ ticks: UInt64) -> UInt64 {
        // Quotient first avoids overflowing the uptime counter on long-lived hosts.
        (ticks / UInt64(denom)) * UInt64(numer) + (ticks % UInt64(denom)) * UInt64(numer) / UInt64(denom)
    }
    func now() -> UInt64 { nanoseconds(mach_absolute_time()) }
}
struct Frame {
    let displayTicks: UInt64
    let receivedNs: UInt64
    let pixels: [UInt8]
}
struct Trial: Codable {
    let destination: Destination
    let downNs: UInt64
    let upNs: UInt64
    let postReturnedNs: UInt64
    let displayTicks: UInt64
    let displayNs: UInt64
    let receivedNs: UInt64
    let latencyMs: Double
    let templateMeanAbsoluteError: Double
}

func attribute(_ item: AXUIElement, _ name: String) -> CFTypeRef? {
    var value: CFTypeRef?
    return AXUIElementCopyAttributeValue(item, name as CFString, &value) == .success ? value : nil
}
func descendants(_ root: AXUIElement) -> [AXUIElement] {
    var result: [AXUIElement] = [], pending = [(root, 0)]
    while let (item, depth) = pending.popLast(), result.count < 1024 {
        result.append(item)
        if depth < 24, let children = attribute(item, kAXChildrenAttribute) as? [AXUIElement] {
            pending.append(contentsOf: children.reversed().map { ($0, depth + 1) })
        }
    }
    return result
}
func geometry(_ item: AXUIElement) throws -> CGRect {
    guard let rawPoint = attribute(item, kAXPositionAttribute), let rawSize = attribute(item, kAXSizeAttribute),
          CFGetTypeID(rawPoint) == AXValueGetTypeID(), CFGetTypeID(rawSize) == AXValueGetTypeID() else { throw ProbeFailure.geometry }
    var point = CGPoint.zero, size = CGSize.zero
    guard AXValueGetValue(unsafeBitCast(rawPoint, to: AXValue.self), .cgPoint, &point),
          AXValueGetValue(unsafeBitCast(rawSize, to: AXValue.self), .cgSize, &size) else { throw ProbeFailure.geometry }
    return CGRect(origin: point, size: size)
}
func text(_ item: AXUIElement) -> String {
    (attribute(item, kAXValueAttribute) as? String) ?? (attribute(item, kAXTitleAttribute) as? String) ?? ""
}
func heading(_ window: AXUIElement, _ destination: Destination) throws -> CGRect {
    let bounds = try geometry(window)
    for row in descendants(window) where attribute(row, kAXRoleAttribute) as? String == "AXStaticText" && destination.isHeading(text(row)) {
        let rect = try geometry(row)
        if rect.minX > bounds.minX + 200, rect.minY > bounds.minY + 100 { return rect }
    }
    throw ProbeFailure.heading
}
func tap(_ window: AXUIElement, _ destination: Destination, _ clock: Clock) throws -> (UInt64, UInt64, UInt64) {
    var pid: pid_t = 0
    guard AXUIElementGetPid(window, &pid) == .success,
          NSWorkspace.shared.frontmostApplication?.processIdentifier == pid,
          attribute(window, kAXMinimizedAttribute) as? Bool == false else { throw ProbeFailure.missingWindow }
    let rows = descendants(window)
    guard let item = rows.first(where: { item in
        ["AXStaticText", "AXButton"].contains(attribute(item, kAXRoleAttribute) as? String ?? "")
            && [kAXTitleAttribute, kAXDescriptionAttribute].contains { attribute(item, $0) as? String == destination.label }
    }) else { throw ProbeFailure.action }
    let bounds = try geometry(window), frame = try geometry(item)
    let point = CGPoint(x: frame.midX, y: frame.midY)
    guard bounds.insetBy(dx: 1, dy: 30).contains(point),
          let move = CGEvent(mouseEventSource: nil, mouseType: .mouseMoved, mouseCursorPosition: point, mouseButton: .left),
          let down = CGEvent(mouseEventSource: nil, mouseType: .leftMouseDown, mouseCursorPosition: point, mouseButton: .left),
          let up = CGEvent(mouseEventSource: nil, mouseType: .leftMouseUp, mouseCursorPosition: point, mouseButton: .left) else { throw ProbeFailure.action }
    move.post(tap: .cghidEventTap)
    let start = clock.now(); down.timestamp = start; down.post(tap: .cghidEventTap)
    let end = clock.now(); up.timestamp = end; up.post(tap: .cghidEventTap)
    return (start, end, clock.now())
}
func distance(_ a: [UInt8], _ b: [UInt8]) -> Double {
    guard a.count == b.count, !a.isEmpty else { return .infinity }
    return Double(zip(a, b).reduce(0) { $0 + abs(Int($1.0) - Int($1.1)) }) / Double(a.count)
}

// Only eight cropped frames are retained. A dedicated queue copies the ROI,
// releases the IOSurface immediately and records overflow rather than hiding it.
final class Frames: NSObject, SCStreamOutput, SCStreamDelegate {
    let clock: Clock, roi: CGRect
    let lock = NSLock()
    var frames: [Frame] = []
    var latest: Frame?
    var dropped = 0
    var idleDropped = 0
    var phase = CapturePhase.resting
    var failure = false
    init(clock: Clock, roi: CGRect) { self.clock = clock; self.roi = roi }
    func stream(_ stream: SCStream, didStopWithError error: Error) {
        lock.lock(); failure = true; lock.unlock()
    }
    func stream(_ stream: SCStream, didOutputSampleBuffer sample: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .screen, sample.isValid,
              let info = CMSampleBufferGetSampleAttachmentsArray(sample, createIfNecessary: false) as? [[SCStreamFrameInfo: Any]],
              let first = info.first, let status = first[.status] as? Int,
              status == SCFrameStatus.complete.rawValue,
              let time = first[.displayTime] as? UInt64,
              let image = sample.imageBuffer else { return }
        let received = clock.now(), displayed = clock.nanoseconds(time)
        guard time > 0, displayed <= received, received - displayed < 5_000_000_000 else {
            lock.lock(); failure = true; lock.unlock(); return
        }
        CVPixelBufferLockBaseAddress(image, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(image, .readOnly) }
        let width = CVPixelBufferGetWidth(image), height = CVPixelBufferGetHeight(image)
        guard CVPixelBufferGetPixelFormatType(image) == kCVPixelFormatType_32BGRA,
              roi.minX >= 0, roi.minY >= 0, roi.maxX <= Double(width), roi.maxY <= Double(height),
              let pointer = CVPixelBufferGetBaseAddress(image) else {
            lock.lock(); failure = true; lock.unlock(); return
        }
        let stride = CVPixelBufferGetBytesPerRow(image), rowBytes = Int(roi.width) * 4
        var pixels = [UInt8](); pixels.reserveCapacity(rowBytes * Int(roi.height))
        for row in Int(roi.minY)..<Int(roi.maxY) {
            let start = pointer.advanced(by: row * stride + Int(roi.minX) * 4).assumingMemoryBound(to: UInt8.self)
            pixels.append(contentsOf: UnsafeBufferPointer(start: start, count: rowBytes))
        }
        let frame = Frame(displayTicks: time, receivedNs: received, pixels: pixels)
        lock.lock(); defer { lock.unlock() }
        if frames.count == 8 {
            frames.removeFirst()
            switch phase { case .resting: idleDropped += 1; case .measuring: dropped += 1 }
        }
        frames.append(frame); latest = frame
    }
    func drain() throws -> [Frame] {
        lock.lock(); defer { lock.unlock() }
        if failure { throw ProbeFailure.stream }
        let result = frames; frames.removeAll(keepingCapacity: true); return result
    }
    func last() throws -> Frame? {
        lock.lock(); defer { lock.unlock() }
        if failure { throw ProbeFailure.stream }
        return latest
    }
    func overflow() -> Int { lock.lock(); defer { lock.unlock() }; return dropped }
    func idleOverflow() -> Int { lock.lock(); defer { lock.unlock() }; return idleDropped }
    func arm() {
        lock.lock(); defer { lock.unlock() }
        frames.removeAll(keepingCapacity: true); phase = .measuring
    }
    func disarm() { lock.lock(); phase = .resting; lock.unlock() }
}
func pause(_ milliseconds: UInt64) async throws { try await Task.sleep(nanoseconds: milliseconds * 1_000_000) }
func save(_ pixels: [UInt8], _ roi: CGRect, _ path: URL) throws {
    let options = CGBitmapInfo.byteOrder32Little.union(CGBitmapInfo(rawValue: CGImageAlphaInfo.premultipliedFirst.rawValue))
    guard let provider = CGDataProvider(data: Data(pixels) as CFData),
          let image = CGImage(width: Int(roi.width), height: Int(roi.height), bitsPerComponent: 8, bitsPerPixel: 32,
                              bytesPerRow: Int(roi.width) * 4, space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: options,
                              provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent),
          let destination = CGImageDestinationCreateWithURL(path as CFURL, UTType.png.identifier as CFString, 1, nil) else { throw ProbeFailure.capture }
    CGImageDestinationAddImage(destination, image, nil)
    guard CGImageDestinationFinalize(destination) else { throw ProbeFailure.capture }
}

@main struct Probe {
    @MainActor static func qualify(_ input: Input) async throws {
        guard input.syntheticOnly, input.pid > 0, input.outputDirectory.hasPrefix("/"),
              AXIsProcessTrusted(), CGPreflightPostEventAccess(), CGPreflightScreenCaptureAccess() else { throw ProbeFailure.permissions }
        let output = URL(fileURLWithPath: input.outputDirectory, isDirectory: true)
        try FileManager.default.createDirectory(at: output, withIntermediateDirectories: false)
        guard let app = NSRunningApplication(processIdentifier: input.pid), app.bundleIdentifier == "ca.homeward.frontend.dev", !app.isTerminated else { throw ProbeFailure.invalidInput }
        app.activate(options: [])
        let application = AXUIElementCreateApplication(input.pid)
        guard let windows = attribute(application, kAXWindowsAttribute) as? [AXUIElement], windows.count == 1, let window = windows.first else { throw ProbeFailure.missingWindow }
        let clock = try Clock(), bounds = try geometry(window), title = try heading(window, .home)
        guard let native = (CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]])?.first(where: {
            $0[kCGWindowOwnerPID as String] as? Int32 == input.pid && $0[kCGWindowLayer as String] as? Int == 0
        }), let id = native[kCGWindowNumber as String] as? UInt32 else { throw ProbeFailure.missingWindow }
        let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: true)
        guard let target = content.windows.first(where: { $0.windowID == id && $0.owningApplication?.processID == input.pid }) else { throw ProbeFailure.missingWindow }
        let filter = SCContentFilter(desktopIndependentWindow: target)
        let scale = Double(filter.pointPixelScale)
        let roi = CGRect(x: floor((title.minX - bounds.minX) * scale), y: floor((title.minY - bounds.minY) * scale),
                         width: floor(min(360, bounds.maxX - title.minX - 10) * scale), height: floor(min(42, title.height) * scale))
        guard roi.width >= 100, roi.height >= 20 else { throw ProbeFailure.geometry }
        let config = SCStreamConfiguration()
        config.width = Int(bounds.width * scale); config.height = Int(bounds.height * scale)
        config.minimumFrameInterval = .zero; config.queueDepth = 3; config.showsCursor = false
        config.capturesAudio = false; config.ignoreShadowsSingleWindow = true
        config.pixelFormat = kCVPixelFormatType_32BGRA
        let receiver = Frames(clock: clock, roi: roi), queue = DispatchQueue(label: "ca.homeward.qualification.compositor")
        let stream = SCStream(filter: filter, configuration: config, delegate: receiver)
        try stream.addStreamOutput(receiver, type: .screen, sampleHandlerQueue: queue)
        try await stream.startCapture()
        do {
            var templates: [Destination: [UInt8]] = [:]
            for destination in [Destination.inventory, .home] {
                let (_, up, _) = try tap(window, destination, clock)
                let deadline = clock.now() + 3_000_000_000
                while true {
                    _ = try receiver.drain()
                    if (try? heading(window, destination)) != nil,
                       let frame = try receiver.last(), clock.nanoseconds(frame.displayTicks) >= up {
                        try await pause(150)
                        guard let settled = try receiver.last() else { throw ProbeFailure.frameTimeout }
                        templates[destination] = settled.pixels
                        try save(settled.pixels, roi, output.appendingPathComponent(destination.rawValue + "-heading.png"))
                        break
                    }
                    guard clock.now() < deadline else { throw ProbeFailure.frameTimeout }
                    try await pause(5)
                }
            }
            guard let home = templates[.home], let inventory = templates[.inventory], distance(home, inventory) > 5 else { throw ProbeFailure.templates }
            var current = Destination.home, trials: [Trial] = []
            let journal = output.appendingPathComponent("trials.jsonl")
            guard FileManager.default.createFile(atPath: journal.path, contents: nil, attributes: [.posixPermissions: 0o600]) else { throw ProbeFailure.invalidInput }
            let records = try FileHandle(forWritingTo: journal)
            defer { try? records.close() }
            // Six warmups are retained but excluded from the reported60sample p95.
            for _ in 0..<66 {
                let destination = current.opposite
                guard let expected = templates[destination], let previous = try receiver.last(), distance(previous.pixels, expected) > 5 else { throw ProbeFailure.templates }
                // Discarding old idle frames cannot hide a transition: arm
                // happens before input; every drop until the match is counted.
                receiver.arm()
                let (down, up, returned) = try tap(window, destination, clock)
                let deadline = clock.now() + 3_000_000_000
                var matched: Frame?
                while matched == nil {
                    for frame in try receiver.drain() where clock.nanoseconds(frame.displayTicks) >= up {
                        if distance(frame.pixels, expected) <= 0.5 { matched = frame; break }
                    }
                    if matched == nil {
                        guard clock.now() < deadline else { throw ProbeFailure.transitionTimeout }
                        try await pause(2)
                    }
                }
                guard let frame = matched else { throw ProbeFailure.transitionTimeout }
                receiver.disarm()
                let displayed = clock.nanoseconds(frame.displayTicks)
                trials.append(Trial(destination: destination, downNs: down, upNs: up, postReturnedNs: returned,
                                    displayTicks: frame.displayTicks, displayNs: displayed, receivedNs: frame.receivedNs,
                                    latencyMs: Double(displayed - down) / 1_000_000, templateMeanAbsoluteError: distance(frame.pixels, expected)))
                if let trial = trials.last {
                    try records.write(contentsOf: JSONEncoder().encode(trial) + Data([10]))
                }
                current = destination
                try await pause(100)
            }
            try await stream.stopCapture()
            let measured = Array(trials.dropFirst(6)), sorted = measured.map(\.latencyMs).sorted()
            let report: [String: Any] = [
                "status": (receiver.overflow() == 0 ? MeasurementStatus.measured : .overflow).rawValue,
                "method": "CGEvent mouse-down timestamp to first matching native heading frame SCStreamFrameInfo.displayTime; mach_absolute_time converted with host timebase",
                "clock": ["numer": clock.numer, "denom": clock.denom], "warmups": 6, "samples": measured.count,
                "p95_ms": sorted[Int(ceil(Double(sorted.count) * 0.95)) - 1], "max_ms": sorted.last ?? 0,
                "capture_queue_depth": 3, "retained_cropped_frames": 8, "capture_overflow": receiver.overflow(),
                "idle_frames_discarded": receiver.idleOverflow(),
                "overflow_scope": "Every drop after arming before input and until the matching frame invalidates the measurement; frames discarded while resting between inputs are counted separately.",
                "window_id": id, "scale": scale, "capture_width": config.width, "capture_height": config.height,
                "roi": ["x": roi.minX, "y": roi.minY, "width": roi.width, "height": roi.height],
                "template_mean_absolute_separation": distance(home, inventory),
                "trials": try JSONSerialization.jsonObject(with: JSONEncoder().encode(trials)),
                "physical_tablet": false, "scope": "Local cached Home/Inventory navigation on this macOS host; compositor presentation, not photon latency; capture overhead included. No cold-start, network action, Android, accessibility or memory verdict."
            ]
            let data = try JSONSerialization.data(withJSONObject: report, options: [.prettyPrinted, .sortedKeys])
            try data.write(to: output.appendingPathComponent("result.json"), options: .withoutOverwriting)
            FileHandle.standardOutput.write(data)
        } catch {
            try? await stream.stopCapture()
            throw error
        }
    }
    static func main() async {
        do {
            let input = try JSONDecoder().decode(Input.self, from: FileHandle.standardInput.readDataToEndOfFile())
            try await qualify(input)
        } catch let error as ProbeFailure {
            FileHandle.standardError.write(Data("Native latency qualification failed: \(error).\n".utf8)); exit(1)
        } catch {
            FileHandle.standardError.write(Data("Native latency qualification failed.\n".utf8)); exit(1)
        }
    }
}

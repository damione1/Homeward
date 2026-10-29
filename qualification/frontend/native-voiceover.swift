import AppKit
import ApplicationServices
import Carbon
import ScreenCaptureKit
import Vision

// Real reader output from its caption window, restricted to an owned synthetic
// Homeward session. No AppleScript automation permission or injected speech.
enum Failure: Error { case input, permissions, readerInUse, window, platform, readerStart, caption, narration, navigation, restore, capture }
enum ReaderCommand: String, Decodable { case qualify, restore }
enum ReaderKey: String, Codable { case toggle, caption, describe = "describe-keyboard-focus", next, interact, activate }
enum Status: String { case passed = "NATIVE_VOICEOVER_PASS", failed = "FAILED", restored = "RESTORED" }
struct Input: Decodable { let command: ReaderCommand; let pid: Int32; let syntheticOnly: Bool; let outputDirectory: String }
struct Narration: Codable { let step: Int; let command: ReaderKey; let text: String; let image: String }

func attribute(_ item: AXUIElement, _ name: String) -> CFTypeRef? {
    var value: CFTypeRef?
    return AXUIElementCopyAttributeValue(item, name as CFString, &value) == .success ? value : nil
}
func descendants(_ root: AXUIElement) -> [AXUIElement] {
    var rows: [AXUIElement] = [], pending = [(root, 0)]
    while let (item, depth) = pending.popLast(), rows.count < 1024 {
        rows.append(item)
        if depth < 24, let children = attribute(item, kAXChildrenAttribute) as? [AXUIElement] {
            pending.append(contentsOf: children.reversed().map { ($0, depth + 1) })
        }
    }
    return rows
}
func geometry(_ item: AXUIElement) throws -> CGRect {
    guard let p = attribute(item, kAXPositionAttribute), let s = attribute(item, kAXSizeAttribute),
          CFGetTypeID(p) == AXValueGetTypeID(), CFGetTypeID(s) == AXValueGetTypeID() else { throw Failure.window }
    var point = CGPoint.zero, size = CGSize.zero
    guard AXValueGetValue(unsafeBitCast(p, to: AXValue.self), .cgPoint, &point),
          AXValueGetValue(unsafeBitCast(s, to: AXValue.self), .cgSize, &size) else { throw Failure.window }
    return CGRect(origin: point, size: size)
}
func write(_ value: Any, _ path: URL) throws {
    try JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys]).write(to: path, options: .withoutOverwriting)
}
func pause(_ milliseconds: UInt64) async throws { try await Task.sleep(nanoseconds: milliseconds * 1_000_000) }
func foreground(_ pid: Int32) throws {
    guard NSWorkspace.shared.frontmostApplication?.processIdentifier == pid else { throw Failure.window }
}
@MainActor func send(_ key: ReaderKey) async throws {
    let codes: [(CGKeyCode, CGEventFlags)], code: CGKeyCode
    switch key {
    case .toggle: codes = [(CGKeyCode(kVK_Command), .maskCommand)]; code = CGKeyCode(kVK_F5)
    case .caption:
        codes = [(CGKeyCode(kVK_Control), .maskControl), (CGKeyCode(kVK_Option), .maskAlternate), (CGKeyCode(kVK_Command), .maskCommand), (CGKeyCode(kVK_Function), .maskSecondaryFn)]
        code = CGKeyCode(kVK_F10)
    case .describe:
        codes = [(CGKeyCode(kVK_Control), .maskControl), (CGKeyCode(kVK_Option), .maskAlternate), (CGKeyCode(kVK_Function), .maskSecondaryFn)]
        code = CGKeyCode(kVK_F4)
    case .next:
        codes = [(CGKeyCode(kVK_Control), .maskControl), (CGKeyCode(kVK_Option), .maskAlternate)]
        code = CGKeyCode(kVK_RightArrow)
    case .interact:
        codes = [(CGKeyCode(kVK_Control), .maskControl), (CGKeyCode(kVK_Option), .maskAlternate), (CGKeyCode(kVK_Shift), .maskShift)]
        code = CGKeyCode(kVK_DownArrow)
    case .activate:
        codes = [(CGKeyCode(kVK_Control), .maskControl), (CGKeyCode(kVK_Option), .maskAlternate)]
        code = CGKeyCode(kVK_Space)
    }
    var flags: CGEventFlags = []
    for (modifier, flag) in codes {
        flags.insert(flag)
        guard let event = CGEvent(keyboardEventSource: nil, virtualKey: modifier, keyDown: true) else { throw Failure.input }
        event.flags = flags; event.post(tap: .cghidEventTap)
        try await pause(10)
    }
    for down in [true, false] {
        guard let event = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: down) else { throw Failure.input }
        event.flags = flags; event.post(tap: .cghidEventTap)
        try await pause(15)
    }
    for (modifier, flag) in codes.reversed() {
        flags.remove(flag)
        guard let event = CGEvent(keyboardEventSource: nil, virtualKey: modifier, keyDown: false) else { throw Failure.input }
        event.flags = flags; event.post(tap: .cghidEventTap)
        try await pause(10)
    }
}
@MainActor func readers() -> [NSRunningApplication] {
    NSWorkspace.shared.runningApplications.filter { $0.bundleIdentifier == "com.apple.VoiceOver" }
}
@MainActor func captionWindow(_ reader: NSRunningApplication) async throws -> SCWindow? {
    let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: true)
    return content.windows.first { window in
        guard window.owningApplication?.processID == reader.processIdentifier else { return false }
        let title = (window.title ?? "").lowercased()
        return title.contains("caption") || title.contains("sous-titr")
    }
}
@MainActor func image(_ window: SCWindow, _ path: URL) async throws -> CGImage {
    let filter = SCContentFilter(desktopIndependentWindow: window), config = SCStreamConfiguration()
    config.width = Int(window.frame.width * Double(filter.pointPixelScale)); config.height = Int(window.frame.height * Double(filter.pointPixelScale))
    config.showsCursor = false; config.ignoreShadowsSingleWindow = true
    let image = try await SCScreenshotManager.captureImage(contentFilter: filter, configuration: config)
    guard let data = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]) else { throw Failure.capture }
    try data.write(to: path, options: .withoutOverwriting)
    return image
}
func recognize(_ image: CGImage) throws -> String {
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = .accurate; request.recognitionLanguages = ["fr-FR", "en-US"]
    try VNImageRequestHandler(cgImage: image).perform([request])
    return (request.results ?? []).prefix(12).compactMap { $0.topCandidates(1).first?.string }.joined(separator: "\n")
}
@MainActor final class ReaderSession {
    let output: URL
    var reader: NSRunningApplication?
    var captionChanged = false
    var didStart = false
    init(_ output: URL) { self.output = output }
    func start() async throws {
        guard !NSWorkspace.shared.isVoiceOverEnabled, readers().isEmpty else { throw Failure.readerInUse }
        try write(["initial_voiceover_enabled": false], output.appendingPathComponent("reader-lease.json"))
        didStart = true
        let configuration = NSWorkspace.OpenConfiguration(); configuration.activates = false
        let started = try await NSWorkspace.shared.openApplication(at: URL(fileURLWithPath: "/System/Library/CoreServices/VoiceOver.app"), configuration: configuration)
        guard started.bundleIdentifier == "com.apple.VoiceOver" else { throw Failure.readerStart }
        reader = started
        try write(["pid": started.processIdentifier], output.appendingPathComponent("owned-reader.json"))
        for _ in 0..<40 {
            if NSWorkspace.shared.isVoiceOverEnabled, !started.isTerminated {
                return
            }
            try await pause(100)
        }
        try write(["voiceover_enabled": NSWorkspace.shared.isVoiceOverEnabled,
                   "owned_process_terminated": started.isTerminated], output.appendingPathComponent("reader-start-failure.json"))
        throw Failure.readerStart
    }
    func stop() async throws {
        guard didStart else { return }
        guard let reader else {
            // Start may have enabled the service before its application was visible.
            if NSWorkspace.shared.isVoiceOverEnabled { try await send(.toggle) }
            guard !NSWorkspace.shared.isVoiceOverEnabled else { throw Failure.restore }
            return
        }
        guard readers().allSatisfy({ $0.processIdentifier == reader.processIdentifier }) else { throw Failure.restore }
        if !reader.isTerminated {
            if captionChanged, NSWorkspace.shared.isVoiceOverEnabled { try await send(.caption) }
            guard reader.terminate() else { throw Failure.restore }
        }
        for _ in 0..<40 {
            if !NSWorkspace.shared.isVoiceOverEnabled, reader.isTerminated || readers().isEmpty {
                let receipt = output.appendingPathComponent("reader-restored.json")
                if !FileManager.default.fileExists(atPath: receipt.path) {
                    try write(["status": Status.restored.rawValue, "voiceover_enabled": false, "owned_process_terminated": true], receipt)
                }
                return
            }
            try await pause(100)
        }
        throw Failure.restore
    }
}

@main struct Probe {
    @MainActor static func qualify(_ input: Input) async throws {
        guard input.syntheticOnly, input.pid > 0, input.outputDirectory.hasPrefix("/") else { throw Failure.input }
        guard AXIsProcessTrusted(), CGPreflightPostEventAccess(), CGPreflightScreenCaptureAccess() else { throw Failure.permissions }
        guard let app = NSRunningApplication(processIdentifier: input.pid), app.bundleIdentifier == "ca.homeward.frontend.dev" else { throw Failure.window }
        try foreground(input.pid)
        let output = URL(fileURLWithPath: input.outputDirectory, isDirectory: true)
        try FileManager.default.createDirectory(at: output, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        let application = AXUIElementCreateApplication(input.pid)
        guard let windows = attribute(application, kAXWindowsAttribute) as? [AXUIElement], windows.count == 1, let window = windows.first,
              let search = descendants(window).first(where: { attribute($0, kAXRoleAttribute) as? String == "AXTextField" && attribute($0, kAXTitleAttribute) as? String == "Rechercher dans ma maison" }) else { throw Failure.window }
        let frame = try geometry(search), bounds = try geometry(window), point = CGPoint(x: frame.midX, y: frame.midY)
        guard bounds.insetBy(dx: 1, dy: 30).contains(point) else { throw Failure.window }
        for kind in [CGEventType.mouseMoved, .leftMouseDown, .leftMouseUp] {
            CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
        }
        try await pause(150)
        let session = ReaderSession(output)
        var signals: [DispatchSourceSignal] = []
        for number in [SIGTERM, SIGINT] {
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
            source.setEventHandler {
                Task { @MainActor in try? await session.stop(); exit(128 + number) }
            }
            source.resume(); signals.append(source)
        }
        defer {
            for source in signals { source.cancel() }
            signal(SIGTERM, SIG_DFL); signal(SIGINT, SIG_DFL)
        }
        var transcript: [Narration] = []
        do {
            try await session.start()
            guard let reader = session.reader else { throw Failure.readerStart }
            try foreground(input.pid)
            var caption = try await captionWindow(reader)
            if caption == nil {
                try await send(.caption); session.captionChanged = true; try await pause(300)
                caption = try await captionWindow(reader)
            }
            guard let caption else {
                let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: true)
                try write(content.windows.filter { $0.owningApplication?.processID == reader.processIdentifier }.map {
                    ["title": $0.title ?? "", "width": $0.frame.width, "height": $0.frame.height, "layer": $0.windowLayer] as [String: Any]
                }, output.appendingPathComponent("reader-windows.json"))
                throw Failure.caption
            }
            func observe(_ step: Int, _ command: ReaderKey) async throws -> String {
                try foreground(input.pid)
                let file = "caption-\(step).png"
                let text = try recognize(await image(caption, output.appendingPathComponent(file)))
                transcript.append(Narration(step: step, command: command, text: text, image: file))
                try JSONEncoder().encode(transcript).write(to: output.appendingPathComponent("transcript.json"), options: .atomic)
                return text
            }
            try await send(.describe); try await pause(500)
            let first = try await observe(0, .describe)
            guard first.localizedCaseInsensitiveContains("Rechercher") else { throw Failure.narration }
            var activated = false, interacted = false
            for step in 1...20 {
                try foreground(input.pid); try await send(.next); try await pause(250)
                let text = try await observe(step, .next)
                if text.localizedCaseInsensitiveContains("Navigation"), !interacted {
                    try await send(.interact); interacted = true; try await pause(200)
                }
                if text.localizedCaseInsensitiveContains("Inventaire") {
                    // A stale caption cannot authorize arbitrary activation.
                    // Require matching keyboard focus in the owned app too.
                    guard descendants(window).contains(where: {
                        attribute($0, kAXFocusedAttribute) as? Bool == true && attribute($0, kAXTitleAttribute) as? String == "Inventaire"
                    }) else { throw Failure.navigation }
                    try await send(.activate); try await pause(300)
                    activated = descendants(window).contains { row in
                        guard attribute(row, kAXRoleAttribute) as? String == "AXStaticText",
                              attribute(row, kAXValueAttribute) as? String == "Inventaire",
                              let rect = try? geometry(row) else { return false }
                        return rect.minX > bounds.minX + 200 && rect.minY > bounds.minY + 100
                    }
                    break
                }
            }
            guard activated else { throw Failure.navigation }
            try await session.stop()
            try write(["status": Status.passed.rawValue, "search_announced": true, "inventory_activated": true,
                       "reader_restored": true, "physical_android": false,
                       "scope": "Actual macOS VoiceOver caption output, bounded cursor navigation and admitted Inventory activation in an owned synthetic window. No all-module or Android reader verdict."], output.appendingPathComponent("result.json"))
        } catch {
            try? await session.stop()
            try? write(["status": Status.failed.rawValue, "failure": String(describing: error as? Failure ?? .platform),
                        "reader_enabled_after_cleanup": NSWorkspace.shared.isVoiceOverEnabled], output.appendingPathComponent("failure.json"))
            throw error
        }
    }
    @MainActor static func restore(_ input: Input) async throws {
        guard input.syntheticOnly, input.outputDirectory.hasPrefix("/") else { throw Failure.input }
        let output = URL(fileURLWithPath: input.outputDirectory, isDirectory: true)
        let lease = try JSONSerialization.jsonObject(with: Data(contentsOf: output.appendingPathComponent("reader-lease.json"))) as? [String: Any]
        guard lease?["initial_voiceover_enabled"] as? Bool == false else { throw Failure.restore }
        if !NSWorkspace.shared.isVoiceOverEnabled, readers().isEmpty { return }
        let owned = try JSONSerialization.jsonObject(with: Data(contentsOf: output.appendingPathComponent("owned-reader.json"))) as? [String: Any]
        guard let pid = owned?["pid"] as? Int32,
              let reader = NSRunningApplication(processIdentifier: pid), reader.bundleIdentifier == "com.apple.VoiceOver" else { throw Failure.restore }
        let session = ReaderSession(output); session.reader = reader; session.didStart = true
        // The normal path restores caption visibility before stopping. A timeout
        // recovery does not blindly toggle a panel with unknown current state.
        try await session.stop()
    }
    static func main() async {
        do {
            let input = try JSONDecoder().decode(Input.self, from: FileHandle.standardInput.readDataToEndOfFile())
            switch input.command { case .qualify: try await qualify(input); case .restore: try await restore(input) }
        } catch let error as Failure {
            FileHandle.standardError.write(Data("Native reader qualification failed: \(error).\n".utf8)); exit(1)
        } catch {
            FileHandle.standardError.write(Data("Native reader qualification failed.\n".utf8)); exit(1)
        }
    }
}

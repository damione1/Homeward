import AppKit
import ApplicationServices
import Carbon
import ScreenCaptureKit

// Native OS qualification, never a product input adapter. Source identifiers
// are observed at the OS boundary; no user editor contents are read here.
enum InputCommand: String, Decodable { case preflight, compose, restore }
struct Input: Decodable { let command: InputCommand; let pid: Int32?; let syntheticOnly: Bool?; let outputDirectory: String? }
struct InputSourceID: Codable { let value: String }
enum KnownInputMode: String { case hiraganaRomaji = "com.apple.inputmethod.Kotoeri.RomajiTyping.Japanese"; case romajiParent = "com.apple.inputmethod.Kotoeri.RomajiTyping" }
enum InputStatus: String { case compositionPass = "COMPOSITION_COMMIT_TAB_PASS", restored = "RESTORED", failed = "FAILED" }
enum ProbeFailure: Error { case input, permissions, window, search, source, composition, focus, restore, capture, externalSourceChange }
enum CompositionPhase: String { case setup, focus, source, composition, commit, tab, clear, restore }
enum KeyChord { case plain, command, control }
struct SourceLease: Codable { let original: InputSourceID; let parentInitiallyEnabled: Bool }

func property(_ source: TISInputSource, _ key: CFString) -> AnyObject? {
    guard let pointer = TISGetInputSourceProperty(source, key) else { return nil }
    return Unmanaged<AnyObject>.fromOpaque(pointer).takeUnretainedValue()
}
func identifier(_ source: TISInputSource) -> InputSourceID? {
    (property(source, kTISPropertyInputSourceID) as? String).map { InputSourceID(value: $0) }
}
func inputSources() -> [TISInputSource] {
    guard let sources = TISCreateInputSourceList(nil, true)?.takeRetainedValue() as? [TISInputSource] else { return [] }
    return sources
}
func inputModes() -> [TISInputSource] { inputSources().filter { property($0, kTISPropertyInputSourceType) as? String == kTISTypeKeyboardInputMode as String } }
func restoreSource(_ lease: SourceLease, _ output: URL) throws {
    let sources = inputSources()
    guard let original = sources.first(where: { identifier($0)?.value == lease.original.value }),
          let parent = sources.first(where: { identifier($0)?.value == KnownInputMode.romajiParent.rawValue }) else { throw ProbeFailure.restore }
    let current = identifier(TISCopyCurrentKeyboardInputSource().takeRetainedValue())?.value
    guard current == lease.original.value || current == KnownInputMode.hiraganaRomaji.rawValue else { throw ProbeFailure.externalSourceChange }
    if current != lease.original.value, TISSelectInputSource(original) != noErr { throw ProbeFailure.restore }
    if !lease.parentInitiallyEnabled, property(parent, kTISPropertyInputSourceIsEnabled) as? Bool == true {
        guard TISDisableInputSource(parent) == noErr else { throw ProbeFailure.restore }
    }
    guard identifier(TISCopyCurrentKeyboardInputSource().takeRetainedValue())?.value == lease.original.value,
          property(parent, kTISPropertyInputSourceIsEnabled) as? Bool == lease.parentInitiallyEnabled else { throw ProbeFailure.restore }
    let record: [String: Any] = ["status": InputStatus.restored.rawValue, "selected_input_source": lease.original.value, "parent_enabled": lease.parentInitiallyEnabled]
    let data = try JSONSerialization.data(withJSONObject: record, options: [.prettyPrinted, .sortedKeys])
    // Idempotent recovery may encounter the normal completion's receipt.
    if !FileManager.default.fileExists(atPath: output.appendingPathComponent("source-restored.json").path) {
        try data.write(to: output.appendingPathComponent("source-restored.json"), options: .withoutOverwriting)
    }
}
func restore(_ input: Input) throws {
    guard let directory = input.outputDirectory, directory.hasPrefix("/") else { throw ProbeFailure.input }
    let output = URL(fileURLWithPath: directory, isDirectory: true)
    let lease = try JSONDecoder().decode(SourceLease.self, from: Data(contentsOf: output.appendingPathComponent("source-lease.json")))
    try restoreSource(lease, output)
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
    guard let p = attribute(item, kAXPositionAttribute), let s = attribute(item, kAXSizeAttribute),
          CFGetTypeID(p) == AXValueGetTypeID(), CFGetTypeID(s) == AXValueGetTypeID() else { throw ProbeFailure.window }
    var point = CGPoint.zero, size = CGSize.zero
    guard AXValueGetValue(unsafeBitCast(p, to: AXValue.self), .cgPoint, &point),
          AXValueGetValue(unsafeBitCast(s, to: AXValue.self), .cgSize, &size) else { throw ProbeFailure.window }
    return CGRect(origin: point, size: size)
}
func ownForeground(_ pid: Int32) throws {
    guard NSWorkspace.shared.frontmostApplication?.processIdentifier == pid else { throw ProbeFailure.window }
}
func key(_ pid: Int32, _ code: CGKeyCode, chord: KeyChord = .plain) async throws {
    try ownForeground(pid)
    let flags: CGEventFlags, modifierKey: CGKeyCode?
    switch chord {
    case .plain: flags = []; modifierKey = nil
    case .command: flags = .maskCommand; modifierKey = CGKeyCode(kVK_Command)
    case .control: flags = .maskControl; modifierKey = CGKeyCode(kVK_Control)
    }
    if let modifierKey {
        let modifier = CGEvent(keyboardEventSource: nil, virtualKey: modifierKey, keyDown: true)
        modifier?.flags = flags; modifier?.post(tap: .cghidEventTap)
    }
    for down in [true, false] {
        guard let event = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: down) else { throw ProbeFailure.input }
        event.flags = flags; event.post(tap: .cghidEventTap)
    }
    if let modifierKey {
        let modifier = CGEvent(keyboardEventSource: nil, virtualKey: modifierKey, keyDown: false)
        modifier?.flags = []; modifier?.post(tap: .cghidEventTap)
    }
    try await Task.sleep(nanoseconds: 30_000_000)
}
func focusSearch(_ window: AXUIElement, _ search: AXUIElement, _ pid: Int32) async throws {
    try ownForeground(pid)
    let bounds = try geometry(window), rect = try geometry(search)
    let point = CGPoint(x: rect.midX, y: rect.midY)
    guard bounds.insetBy(dx: 1, dy: 30).contains(point) else { throw ProbeFailure.window }
    for kind in [CGEventType.mouseMoved, .leftMouseDown, .leftMouseUp] {
        guard let event = CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left) else { throw ProbeFailure.input }
        event.post(tap: .cghidEventTap)
    }
    try await Task.sleep(nanoseconds: 100_000_000)
    guard attribute(search, kAXFocusedAttribute) as? Bool == true else { throw ProbeFailure.focus }
}
@MainActor func capture(_ pid: Int32, _ path: URL) async throws {
    let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: true)
    guard let window = content.windows.first(where: { $0.owningApplication?.processID == pid && $0.windowLayer == 0 }) else { throw ProbeFailure.capture }
    let filter = SCContentFilter(desktopIndependentWindow: window), config = SCStreamConfiguration()
    config.width = Int(window.frame.width * Double(filter.pointPixelScale))
    config.height = Int(window.frame.height * Double(filter.pointPixelScale))
    config.showsCursor = false; config.ignoreShadowsSingleWindow = true
    let image = try await SCScreenshotManager.captureImage(contentFilter: filter, configuration: config)
    guard let data = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]) else { throw ProbeFailure.capture }
    try data.write(to: path, options: .withoutOverwriting)
}
@MainActor func compose(_ input: Input) async throws {
    guard let pid = input.pid, pid > 0, input.syntheticOnly == true,
          let directory = input.outputDirectory, directory.hasPrefix("/") else { throw ProbeFailure.input }
    guard AXIsProcessTrusted(), CGPreflightPostEventAccess(), CGPreflightScreenCaptureAccess() else { throw ProbeFailure.permissions }
    guard let app = NSRunningApplication(processIdentifier: pid), app.bundleIdentifier == "ca.homeward.frontend.dev", !app.isTerminated else { throw ProbeFailure.window }
    try ownForeground(pid)
    let application = AXUIElementCreateApplication(pid)
    guard let windows = attribute(application, kAXWindowsAttribute) as? [AXUIElement], windows.count == 1, let window = windows.first,
          let search = descendants(window).first(where: {
              attribute($0, kAXRoleAttribute) as? String == "AXTextField"
                && attribute($0, kAXTitleAttribute) as? String == "Rechercher dans ma maison"
          }), attribute(search, kAXValueAttribute) as? String == "" else { throw ProbeFailure.search }
    let output = URL(fileURLWithPath: directory, isDirectory: true)
    try FileManager.default.createDirectory(at: output, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    let original = TISCopyCurrentKeyboardInputSource().takeRetainedValue()
    guard let originalID = identifier(original),
          let mode = inputModes().first(where: { identifier($0)?.value == KnownInputMode.hiraganaRomaji.rawValue }),
          let parent = inputSources().first(where: { identifier($0)?.value == KnownInputMode.romajiParent.rawValue }),
          property(mode, kTISPropertyInputSourceIsEnabled) as? Bool == true,
          let parentEnabled = property(parent, kTISPropertyInputSourceIsEnabled) as? Bool else { throw ProbeFailure.source }
    let lease = SourceLease(original: originalID, parentInitiallyEnabled: parentEnabled)
    try JSONEncoder().encode(lease).write(to: output.appendingPathComponent("source-lease.json"), options: .withoutOverwriting)
    // The lease precedes changes. SIGTERM from the owning harness restores OS
    // state; an explicit recovery command also supports an interrupted proof.
    var signals: [DispatchSourceSignal] = []
    for number in [SIGTERM, SIGINT] {
        signal(number, SIG_IGN)
        let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
        source.setEventHandler { try? restoreSource(lease, output); exit(128 + number) }
        source.resume(); signals.append(source)
    }
    var restored = false, phase = CompositionPhase.setup
    var sourceTransitions: [String] = []
    defer {
        if !restored { try? restoreSource(lease, output) }
        for source in signals { source.cancel() }
        signal(SIGTERM, SIG_DFL); signal(SIGINT, SIG_DFL)
    }
    do {
        phase = .focus; try await focusSearch(window, search, pid)
        phase = .source
        if !parentEnabled { guard TISEnableInputSource(parent) == noErr else { throw ProbeFailure.source } }
        // Select through the real OS shortcut in the foreground document.
        // TISSelectInputSource from an external probe can leave that document's
        // NSTextInputContext on its previous source despite a successful return.
        for _ in 0..<6 {
            try await key(pid, CGKeyCode(kVK_Space), chord: .control)
            try await Task.sleep(nanoseconds: 150_000_000)
            let observed = identifier(TISCopyCurrentKeyboardInputSource().takeRetainedValue())?.value ?? ""
            sourceTransitions.append(observed)
            if observed == KnownInputMode.hiraganaRomaji.rawValue { break }
        }
        guard identifier(TISCopyCurrentKeyboardInputSource().takeRetainedValue())?.value == KnownInputMode.hiraganaRomaji.rawValue else { throw ProbeFailure.source }
        phase = .composition
        // Physical keys through the native input method, not Unicode injection,
        // AXSetValue or a direct Slint preedit/commit callback.
        var keyTrace: [[String: Any]] = []
        // Vowels avoid input-method-specific handling of doubled N. The full
        // expected string still rejects an ASCII prefix or duplicated commit.
        for code in [kVK_ANSI_A, kVK_ANSI_I, kVK_ANSI_U, kVK_ANSI_E, kVK_ANSI_O] {
            guard identifier(TISCopyCurrentKeyboardInputSource().takeRetainedValue())?.value == KnownInputMode.hiraganaRomaji.rawValue else { throw ProbeFailure.source }
            try await key(pid, CGKeyCode(code))
            keyTrace.append(["virtual_key": code, "search_ax_value": attribute(search, kAXValueAttribute) as? String ?? ""])
        }
        let trace = try JSONSerialization.data(withJSONObject: keyTrace, options: [.prettyPrinted, .sortedKeys])
        try trace.write(to: output.appendingPathComponent("key-trace.json"), options: .withoutOverwriting)
        try await key(pid, CGKeyCode(kVK_F6)) // Native IME conversion to Hiragana.
        try await Task.sleep(nanoseconds: 200_000_000)
        let preeditValue = attribute(search, kAXValueAttribute) as? String ?? ""
        try await capture(pid, output.appendingPathComponent("composition-before-commit.png"))
        phase = .commit; try await key(pid, CGKeyCode(kVK_Return))
        try await Task.sleep(nanoseconds: 150_000_000)
        let committed = attribute(search, kAXValueAttribute) as? String ?? ""
        guard committed == "あいうえお" else { throw ProbeFailure.composition }
        phase = .tab; try await key(pid, CGKeyCode(kVK_Tab))
        try await Task.sleep(nanoseconds: 100_000_000)
        guard attribute(search, kAXFocusedAttribute) as? Bool == false,
              attribute(search, kAXValueAttribute) as? String == committed else { throw ProbeFailure.focus }
        try await capture(pid, output.appendingPathComponent("committed-after-tab.png"))
        phase = .clear; try await focusSearch(window, search, pid)
        try await key(pid, CGKeyCode(kVK_ANSI_A), chord: .command)
        try await key(pid, CGKeyCode(kVK_Delete))
        guard attribute(search, kAXValueAttribute) as? String == "" else { throw ProbeFailure.search }
        phase = .restore
        try restoreSource(lease, output); restored = true
        let report: [String: Any] = ["status": InputStatus.compositionPass.rawValue, "input_method": KnownInputMode.hiraganaRomaji.rawValue,
            "original_input_source": originalID.value, "restored_input_source": originalID.value,
            "parent_initially_enabled": parentEnabled, "parent_restored": true,
            "source_switches": sourceTransitions,
            "preedit_ax_value": preeditValue, "committed_value": committed, "commit_survived_tab": true,
            "event_path": "CGHID event tap with owned foreground checked before input; physical virtual keys, no text injection",
            "search_cleared": true, "reader_qualified": false, "physical_android": false,
            "scope": "Native macOS Kotoeri composition in the owned Homeward search field, commit/focus transition/source restoration. No full keyboard, screen-reader or Android verdict."]
        let data = try JSONSerialization.data(withJSONObject: report, options: [.prettyPrinted, .sortedKeys])
        try data.write(to: output.appendingPathComponent("result.json"), options: .withoutOverwriting)
        FileHandle.standardOutput.write(data)
    } catch {
        try? await capture(pid, output.appendingPathComponent("failure-before-source-restore.png"))
        let report: [String: Any] = ["status": InputStatus.failed.rawValue, "phase": phase.rawValue, "original_input_source": originalID.value,
                                    "search_ax_value": attribute(search, kAXValueAttribute) as? String ?? "",
                                    "source_switches": sourceTransitions, "parent_initially_enabled": parentEnabled,
                                    "selected_at_failure": identifier(TISCopyCurrentKeyboardInputSource().takeRetainedValue())?.value ?? ""]
        if let data = try? JSONSerialization.data(withJSONObject: report, options: [.prettyPrinted, .sortedKeys]) {
            try? data.write(to: output.appendingPathComponent("failure.json"), options: .withoutOverwriting)
        }
        throw error
    }
}
func preflight() throws {
    let sources: [[String: Any]] = inputModes().compactMap { source in
        guard let id = identifier(source), id.value.hasPrefix("com.apple.inputmethod.Kotoeri") else { return nil }
        return ["id": id.value, "name": property(source, kTISPropertyLocalizedName) as? String ?? "",
                "enabled": property(source, kTISPropertyInputSourceIsEnabled) as? Bool ?? false,
                "selectable": property(source, kTISPropertyInputSourceIsSelectCapable) as? Bool ?? false]
    }
    let current = TISCopyCurrentKeyboardInputSource().takeRetainedValue()
    let data: [String: Any] = [
        "selected_input_source": identifier(current)?.value ?? "",
        "japanese_input_modes": sources,
        "accessibility_trusted": AXIsProcessTrusted(), "post_events_allowed": CGPreflightPostEventAccess(),
        "screen_capture_allowed": CGPreflightScreenCaptureAccess(),
        "voiceover_running": NSWorkspace.shared.runningApplications.contains { $0.bundleIdentifier == "com.apple.VoiceOver" },
        "reader_or_ime_qualified": false
    ]
    FileHandle.standardOutput.write(try JSONSerialization.data(withJSONObject: data, options: [.prettyPrinted, .sortedKeys]))
}
@main struct Probe {
    @MainActor static func main() async {
        do {
            let input = try JSONDecoder().decode(Input.self, from: FileHandle.standardInput.readDataToEndOfFile())
            switch input.command { case .preflight: try preflight(); case .compose: try await compose(input); case .restore: try restore(input) }
        } catch let error as ProbeFailure {
            FileHandle.standardError.write(Data("Native input qualification failed: \(error).\n".utf8)); exit(1)
        } catch {
            FileHandle.standardError.write(Data("Native input qualification failed.\n".utf8)); exit(1)
        }
    }
}

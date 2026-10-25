import Foundation
import AppKit
import ApplicationServices
import CoreGraphics

// Qualification only: address one owned process/window, never enumerate other
// applications, inspect editor values, or retain credentials in input files.
enum Command: String, Decodable {
    case info, click, close, minimize, restore, focus
    case tap, resize, key, scroll, tree, press, release
    case fillLogin = "fill-login", read, signOut = "sign-out", scrollEnd = "scroll-end", checkRow = "check-row"
}
enum NativeKey: String, Decodable {
    case tab, backtab, up, down, home, end, enter, escape, space
    case letterK = "letter-k", letterI = "letter-i", letterT = "letter-t", letterC = "letter-c"
    case letterH = "letter-h", letterE = "letter-e", letterN = "letter-n"
}
struct Input: Decodable {
    let command: Command; let pid: Int32; let x: Double?; let y: Double?
    let syntheticOnly: Bool?; let loginName: String?; let password: String?
    let expectedRowLabel: String?
    let label: String?; let key: NativeKey?
    let width: Double?; let height: Double?; let delta: Int32?
    let fraction: Double?
}
enum ProbeFailure: Error { case invalidInput, missingWindow, accessibilityUnavailable }

func attribute(_ element: AXUIElement, _ name: CFString) -> CFTypeRef? {
    var value: CFTypeRef?
    return AXUIElementCopyAttributeValue(element, name, &value) == .success ? value : nil
}
func ownedWindow(_ pid: Int32) -> [String: Any]? {
    let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
    return windows.first { ($0[kCGWindowOwnerPID as String] as? Int32) == pid && ($0[kCGWindowLayer as String] as? Int) == 0 }
}
func descendants(_ root: AXUIElement) -> [AXUIElement] {
    var result = [AXUIElement]()
    var pending = [(root, 0)]
    while let (element, depth) = pending.popLast(), result.count < 1024 {
        result.append(element)
        if depth < 24, let children = attribute(element, kAXChildrenAttribute as CFString) as? [AXUIElement] {
            pending.append(contentsOf: children.reversed().map { ($0, depth + 1) })
        }
    }
    return result
}
func action(_ window: AXUIElement, _ label: String) -> AXUIElement? {
    descendants(window).first { item in
        let role = attribute(item, kAXRoleAttribute as CFString) as? String ?? ""
        return ["AXButton", "AXRow", "AXCell", "AXMenuItem", "AXRadioButton", "AXTextField", "AXStaticText"].contains(role)
            && [kAXTitleAttribute, kAXDescriptionAttribute].contains { attribute(item, $0 as CFString) as? String == label }
    }
}
func geometry(_ element: AXUIElement) throws -> CGRect {
    guard let rawPosition = attribute(element, kAXPositionAttribute as CFString), let rawSize = attribute(element, kAXSizeAttribute as CFString),
          CFGetTypeID(rawPosition) == AXValueGetTypeID(), CFGetTypeID(rawSize) == AXValueGetTypeID() else { throw ProbeFailure.invalidInput }
    var position = CGPoint.zero; var size = CGSize.zero
    guard AXValueGetValue(unsafeBitCast(rawPosition, to: AXValue.self), .cgPoint, &position),
          AXValueGetValue(unsafeBitCast(rawSize, to: AXValue.self), .cgSize, &size) else { throw ProbeFailure.invalidInput }
    return CGRect(origin: position, size: size)
}
func tap(_ window: AXUIElement, _ label: String) throws {
    guard let target = action(window, label) else { throw ProbeFailure.invalidInput }
    let frame = try geometry(target), bounds = try geometry(window)
    let point = CGPoint(x: frame.midX, y: frame.midY)
    let mainHeight = NSScreen.screens.first?.frame.height ?? 0
    guard bounds.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: { screen in
        CGRect(x: screen.frame.minX, y: mainHeight - screen.frame.maxY, width: screen.frame.width, height: screen.frame.height).contains(point)
    }) else { throw ProbeFailure.invalidInput }
    for kind in [CGEventType.mouseMoved, .leftMouseDown, .leftMouseUp] {
        CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.05)
    }
}
func navigate(_ window: AXUIElement, _ label: String) throws {
    if action(window, label) == nil {
        try press(window, "Navigation")
        Thread.sleep(forTimeInterval: 0.15)
    }
    try tap(window, label)
    Thread.sleep(forTimeInterval: 0.15)
}
func sendKey(_ pid: Int32, _ key: NativeKey) throws {
    let code: CGKeyCode, flags: CGEventFlags
    switch key {
    case .tab: code = 48; flags = []
    case .backtab: code = 48; flags = .maskShift
    case .up: code = 126; flags = []
    case .down: code = 125; flags = []
    case .home: code = 115; flags = []
    case .end: code = 119; flags = []
    case .enter: code = 36; flags = []
    case .escape: code = 53; flags = []
    case .space: code = 49; flags = []
    case .letterK: code = 40; flags = []
    case .letterI: code = 34; flags = []
    case .letterT: code = 17; flags = []
    case .letterC: code = 8; flags = []
    case .letterH: code = 4; flags = []
    case .letterE: code = 14; flags = []
    case .letterN: code = 45; flags = []
    }
    for down in [true, false] {
        guard let event = CGEvent(keyboardEventSource: CGEventSource(stateID: .hidSystemState), virtualKey: code, keyDown: down) else { throw ProbeFailure.invalidInput }
        event.flags = flags; event.postToPid(pid)
        Thread.sleep(forTimeInterval: 0.06)
    }
}
func press(_ window: AXUIElement, _ label: String) throws {
    guard let button = descendants(window).first(where: { element in
        guard attribute(element, kAXRoleAttribute as CFString) as? String == kAXButtonRole as String else { return false }
        return [kAXTitleAttribute, kAXDescriptionAttribute].contains { name in
            attribute(element, name as CFString) as? String == label
        }
    }) else { throw ProbeFailure.missingWindow }
    _ = AXUIElementSetAttributeValue(button, kAXFocusedAttribute as CFString, kCFBooleanTrue)
    guard AXUIElementPerformAction(button, kAXPressAction as CFString) == .success else { throw ProbeFailure.invalidInput }
}
func projectedRowPresent(_ table: AXUIElement, _ label: String) -> Bool {
    descendants(table).contains { element in
        [kAXTitleAttribute, kAXDescriptionAttribute].contains { name in
            attribute(element, name as CFString) as? String == label
        }
    }
}
func activate(_ application: NSRunningApplication, _ window: AXUIElement) {
    // The test app's previous native placement can extend above the display.
    // Stabilize only this owned window before sending relative input events.
    var position = CGPoint(x: 40, y: 40)
    if let value = AXValueCreate(.cgPoint, &position) {
        _ = AXUIElementSetAttributeValue(window, kAXPositionAttribute as CFString, value)
    }
    // Reasserting AX window focus while it is already active can reset the
    // widget's keyboard focus. A focus check must preserve the current editor.
    if NSWorkspace.shared.frontmostApplication?.processIdentifier == application.processIdentifier {
        return
    }
    application.activate(options: [])
    _ = AXUIElementPerformAction(window, kAXRaiseAction as CFString)
    _ = AXUIElementSetAttributeValue(window, kAXMainAttribute as CFString, kCFBooleanTrue)
    _ = AXUIElementSetAttributeValue(window, kAXFocusedAttribute as CFString, kCFBooleanTrue)
    Thread.sleep(forTimeInterval: 0.15)
}
do {
    guard CommandLine.arguments.count == 2 else { throw ProbeFailure.invalidInput }
    let fromStdin = CommandLine.arguments[1] == "-"
    let data = try fromStdin ? FileHandle.standardInput.readToEnd() ?? Data() : Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1]))
    let input = try JSONDecoder().decode(Input.self, from: data)
    guard let app = NSRunningApplication(processIdentifier: input.pid), !app.isTerminated else { throw ProbeFailure.missingWindow }
    let application = AXUIElementCreateApplication(input.pid)
    switch input.command {
    case .info:
        guard let native = ownedWindow(input.pid), let bounds = native[kCGWindowBounds as String] as? [String: Any] else { throw ProbeFailure.missingWindow }
        let data = try JSONSerialization.data(withJSONObject: ["pid": input.pid, "window_id": native[kCGWindowNumber as String] ?? 0, "bounds": bounds, "accessibility_trusted": AXIsProcessTrusted(), "post_events_allowed": CGPreflightPostEventAccess()], options: [.sortedKeys])
        print(String(decoding: data, as: UTF8.self))
    case .click, .close, .minimize, .restore, .focus, .fillLogin, .read, .signOut, .scrollEnd, .checkRow, .tap, .resize, .key, .scroll, .tree, .press, .release:
        guard AXIsProcessTrusted(), let windows = attribute(application, kAXWindowsAttribute as CFString) as? [AXUIElement], let window = windows.first else { throw ProbeFailure.accessibilityUnavailable }
        if case .restore = input.command {
            guard AXUIElementSetAttributeValue(window, kAXMinimizedAttribute as CFString, kCFBooleanFalse) == .success else { throw ProbeFailure.invalidInput }
            app.activate(options: [])
            _ = AXUIElementPerformAction(window, kAXRaiseAction as CFString)
            Thread.sleep(forTimeInterval: 0.25)
        }
        activate(app, window)
        guard NSWorkspace.shared.frontmostApplication?.processIdentifier == input.pid else { throw ProbeFailure.missingWindow }
        switch input.command {
        case .info: throw ProbeFailure.invalidInput
        case .focus: break
        case .tap:
            guard input.syntheticOnly == true, let label = input.label else { throw ProbeFailure.invalidInput }
            try tap(window, label)
        case .press:
            guard input.syntheticOnly == true, let label = input.label,
                  let fraction = input.fraction, fraction >= 0, fraction <= 1,
                  let slider = descendants(window).first(where: {
                      attribute($0, kAXRoleAttribute as CFString) as? String == "AXSlider"
                      && attribute($0, kAXTitleAttribute as CFString) as? String == label
                  }) else { throw ProbeFailure.invalidInput }
            let frame = try geometry(slider), bounds = try geometry(window)
            let point = CGPoint(x: frame.minX + 12 + (frame.width - 24) * fraction, y: frame.midY)
            let mainHeight = NSScreen.screens.first?.frame.height ?? 0
            guard frame.width >= 48, bounds.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: {
                CGRect(x: $0.frame.minX, y: mainHeight - $0.frame.maxY, width: $0.frame.width, height: $0.frame.height).contains(point)
            }) else { throw ProbeFailure.invalidInput }
            for kind in [CGEventType.mouseMoved, .leftMouseDown] {
                CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
                Thread.sleep(forTimeInterval: 0.05)
            }
            let result = try JSONSerialization.data(withJSONObject: ["x": point.x, "y": point.y])
            FileHandle.standardOutput.write(result)
        case .release:
            guard input.syntheticOnly == true, let x = input.x, let y = input.y,
                  (try geometry(window)).insetBy(dx: 1, dy: 30).contains(CGPoint(x: x, y: y)) else { throw ProbeFailure.invalidInput }
            CGEvent(mouseEventSource: nil, mouseType: .leftMouseUp, mouseCursorPosition: CGPoint(x: x, y: y), mouseButton: .left)?.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.05)
        case .resize:
            guard input.syntheticOnly == true, let width = input.width, let height = input.height,
                  width >= 320, width <= 2000, height >= 400, height <= 2000 else { throw ProbeFailure.invalidInput }
            var size = CGSize(width: width, height: height + 28), position = CGPoint(x: 40, y: 40)
            guard let s = AXValueCreate(.cgSize, &size), let p = AXValueCreate(.cgPoint, &position),
                  AXUIElementSetAttributeValue(window, kAXSizeAttribute as CFString, s) == .success,
                  AXUIElementSetAttributeValue(window, kAXPositionAttribute as CFString, p) == .success else { throw ProbeFailure.invalidInput }
        case .key:
            guard input.syntheticOnly == true, let key = input.key else { throw ProbeFailure.invalidInput }
            try sendKey(input.pid, key)
        case .scroll:
            guard input.syntheticOnly == true, let delta = input.delta, abs(Int64(delta)) <= 1000 else { throw ProbeFailure.invalidInput }
            let frame = try geometry(window), point = CGPoint(x: frame.midX, y: frame.midY)
            let mainHeight = NSScreen.screens.first?.frame.height ?? 0
            guard NSScreen.screens.contains(where: { CGRect(x: $0.frame.minX, y: mainHeight - $0.frame.maxY, width: $0.frame.width, height: $0.frame.height).contains(point) }) else { throw ProbeFailure.invalidInput }
            CGEvent(mouseEventSource: nil, mouseType: .mouseMoved, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.05)
            let event = CGEvent(scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 1, wheel1: delta, wheel2: 0, wheel3: 0)
            event?.location = point; event?.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.15)
        case .tree:
            guard input.syntheticOnly == true else { throw ProbeFailure.invalidInput }
            let rows: [[String: Any]] = descendants(window).map { item in
                let bounds = try? geometry(item)
                let role = attribute(item, kAXRoleAttribute as CFString) as? String ?? ""
                return ["role": role,
                        "title": attribute(item, kAXTitleAttribute as CFString) as? String ?? "",
                        "description": attribute(item, kAXDescriptionAttribute as CFString) as? String ?? "",
                        // AX static text may expose its display label as value.
                        // Never read an editable or secure text-field value.
                        "text": role == "AXStaticText" ? attribute(item, kAXValueAttribute as CFString) as? String ?? "" : "",
                        "focused": attribute(item, kAXFocusedAttribute as CFString) as? Bool ?? false,
                        "enabled": attribute(item, kAXEnabledAttribute as CFString) as? Bool ?? false,
                        "x": bounds?.minX ?? 0, "y": bounds?.minY ?? 0,
                        "width": bounds?.width ?? 0, "height": bounds?.height ?? 0]
            }
            print(String(decoding: try JSONSerialization.data(withJSONObject: rows, options: [.sortedKeys]), as: UTF8.self))
        case .fillLogin:
            // Write only fixture values supplied over stdin; never read an
            // editor's value or store the synthetic password in an action file.
            guard fromStdin, input.syntheticOnly == true,
                  let login = input.loginName, let password = input.password,
                  !login.isEmpty, !password.isEmpty, login.count <= 1024, password.count <= 1024 else { throw ProbeFailure.invalidInput }
            let editors = descendants(window).filter { attribute($0, kAXRoleAttribute as CFString) as? String == kAXTextFieldRole as String }
            guard editors.count == 2 else { throw ProbeFailure.invalidInput }
            for (editor, value) in zip(editors, [login, password]) {
                guard AXUIElementSetAttributeValue(editor, kAXValueAttribute as CFString, value as CFString) == .success else { throw ProbeFailure.invalidInput }
            }
            Thread.sleep(forTimeInterval: 0.2)
            try press(window, "Se connecter")
        case .read:
            try press(window, "Afficher ma maison")
            try navigate(window, "Inventaire")
        case .signOut:
            try navigate(window, "Préférences")
            try press(window, "Se déconnecter")
        case .checkRow:
            if !descendants(window).contains(where: { attribute($0, kAXRoleAttribute as CFString) as? String == kAXTableRole as String }) { try navigate(window, "Inventaire") }
            guard input.syntheticOnly == true, let label = input.expectedRowLabel, !label.isEmpty, label.count <= 1024,
                  let table = descendants(window).first(where: { attribute($0, kAXRoleAttribute as CFString) as? String == kAXTableRole as String }),
                  projectedRowPresent(table, label) else { throw ProbeFailure.invalidInput }
            print("{\"expected_row_projected\":true}")
        case .scrollEnd:
            guard input.syntheticOnly == true, let label = input.expectedRowLabel, !label.isEmpty, label.count <= 1024,
                  let table = descendants(window).first(where: { attribute($0, kAXRoleAttribute as CFString) as? String == kAXTableRole as String }),
                  let rawPosition = attribute(table, kAXPositionAttribute as CFString),
                  let rawSize = attribute(table, kAXSizeAttribute as CFString),
                  CFGetTypeID(rawPosition) == AXValueGetTypeID(), CFGetTypeID(rawSize) == AXValueGetTypeID() else { throw ProbeFailure.invalidInput }
            var position = CGPoint.zero; var size = CGSize.zero
            guard AXValueGetValue(unsafeBitCast(rawPosition, to: AXValue.self), .cgPoint, &position),
                  AXValueGetValue(unsafeBitCast(rawSize, to: AXValue.self), .cgSize, &size),
                  size.width > 0, size.height > 0 else { throw ProbeFailure.invalidInput }
            let point = CGPoint(x: position.x + size.width / 2, y: position.y + min(size.height / 2, 100))
            // A real row click now opens its read-only detail. Returning through
            // the product restores the table's focus before sending End.
            for type in [CGEventType.mouseMoved, .leftMouseDown, .leftMouseUp] {
                guard let event = CGEvent(mouseEventSource: nil, mouseType: type, mouseCursorPosition: point, mouseButton: .left) else { throw ProbeFailure.missingWindow }
                event.setIntegerValueField(.mouseEventClickState, value: 1)
                event.post(tap: .cghidEventTap)
                RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05))
            }
            let detailDeadline = Date(timeIntervalSinceNow: 3)
            while Date() < detailDeadline && action(window, "Retour aux éléments") == nil {
                Thread.sleep(forTimeInterval: 0.05)
            }
            try press(window, "Retour aux éléments")
            try sendKey(input.pid, .end)
            // AX elements can be replaced when a view hides and returns.
            // Resolve the live table again instead of retaining its old proxy.
            func lastRowPresent() -> Bool {
                guard let current = descendants(window).first(where: { attribute($0, kAXRoleAttribute as CFString) as? String == kAXTableRole as String }) else { return false }
                return projectedRowPresent(current, label)
            }
            let deadline = Date(timeIntervalSinceNow: 3)
            while Date() < deadline && !lastRowPresent() {
                RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.1))
            }
            guard lastRowPresent() else { throw ProbeFailure.invalidInput }
            print("{\"expected_last_row_projected\":true,\"native_row_click_and_end\":true}")
        case .minimize:
            guard AXUIElementSetAttributeValue(window, kAXMinimizedAttribute as CFString, kCFBooleanTrue) == .success else { throw ProbeFailure.invalidInput }
        case .restore:
            guard attribute(window, kAXMinimizedAttribute as CFString) as? Bool == false else { throw ProbeFailure.invalidInput }
        case .close:
            guard let raw = attribute(window, kAXCloseButtonAttribute as CFString), CFGetTypeID(raw) == AXUIElementGetTypeID() else { throw ProbeFailure.missingWindow }
            let button = unsafeBitCast(raw, to: AXUIElement.self)
            guard AXUIElementPerformAction(button, kAXPressAction as CFString) == .success else { throw ProbeFailure.invalidInput }
        case .click:
            guard let px = input.x, let py = input.y, let active = ownedWindow(input.pid),
                  let box = active[kCGWindowBounds as String] as? [String: Any],
                  let x = box["X"] as? Double, let y = box["Y"] as? Double,
                  let width = box["Width"] as? Double, let height = box["Height"] as? Double,
                  px >= 0, py >= 0, px < width, py < height else { throw ProbeFailure.invalidInput }
            let point = CGPoint(x: x + px, y: y + py)
            CGEvent(mouseEventSource: nil, mouseType: .mouseMoved, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
            for kind in [CGEventType.leftMouseDown, CGEventType.leftMouseUp] {
                let event = CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)
                event?.setIntegerValueField(.mouseEventClickState, value: 1)
                event?.post(tap: .cghidEventTap)
                Thread.sleep(forTimeInterval: 0.05)
            }
        }
    }
} catch {
    fputs("Owned native UI qualification failed.\n", stderr)
    exit(1)
}

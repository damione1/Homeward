import Foundation
import AppKit
import ApplicationServices
import CoreGraphics

// Qualification only: address one owned process/window, never enumerate other
// applications, inspect editor values, or retain credentials in input files.
enum Command: String, Decodable {
    case info, click, close, minimize, restore, focus
    case fillLogin = "fill-login", read, signOut = "sign-out", scrollEnd = "scroll-end", checkRow = "check-row"
}
struct Input: Decodable {
    let command: Command; let pid: Int32; let x: Double?; let y: Double?
    let syntheticOnly: Bool?; let loginName: String?; let password: String?
    let expectedRowLabel: String?
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
    while let (element, depth) = pending.popLast(), result.count < 512 {
        result.append(element)
        if depth < 16, let children = attribute(element, kAXChildrenAttribute as CFString) as? [AXUIElement] {
            pending.append(contentsOf: children.reversed().map { ($0, depth + 1) })
        }
    }
    return result
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
    case .click, .close, .minimize, .restore, .focus, .fillLogin, .read, .signOut, .scrollEnd, .checkRow:
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
        case .read: try press(window, "Vérifier l’accès aux données")
        case .signOut: try press(window, "Se déconnecter")
        case .checkRow:
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
            // Focus through a real row click, then send the native End key.
            // This fixture has read-only rows: activation only takes focus.
            // No direct model/viewport mutation is used by qualification.
            for type in [CGEventType.leftMouseDown, .leftMouseUp] {
                guard let event = CGEvent(mouseEventSource: nil, mouseType: type, mouseCursorPosition: point, mouseButton: .left) else { throw ProbeFailure.missingWindow }
                event.post(tap: .cghidEventTap)
                RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05))
            }
            for down in [true, false] {
                guard NSWorkspace.shared.frontmostApplication?.processIdentifier == input.pid,
                      let event = CGEvent(keyboardEventSource: nil, virtualKey: 119, keyDown: down) else { throw ProbeFailure.missingWindow }
                event.post(tap: .cghidEventTap)
                RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05))
            }
            let deadline = Date(timeIntervalSinceNow: 3)
            while Date() < deadline && !projectedRowPresent(table, label) {
                RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.1))
            }
            guard projectedRowPresent(table, label) else { throw ProbeFailure.invalidInput }
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

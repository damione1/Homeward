import Foundation
import AppKit
import ApplicationServices
import CoreGraphics

// Qualification only: address one owned process/window, never enumerate other
// applications, inspect editor values, or retain credentials in input files.
enum Command: String, Decodable { case info, click, close }
struct Input: Decodable { let command: Command; let pid: Int32; let x: Double?; let y: Double? }
enum ProbeFailure: Error { case invalidInput, missingWindow, accessibilityUnavailable }

func attribute(_ element: AXUIElement, _ name: CFString) -> CFTypeRef? {
    var value: CFTypeRef?
    return AXUIElementCopyAttributeValue(element, name, &value) == .success ? value : nil
}
func ownedWindow(_ pid: Int32) -> [String: Any]? {
    let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
    return windows.first { ($0[kCGWindowOwnerPID as String] as? Int32) == pid && ($0[kCGWindowLayer as String] as? Int) == 0 }
}
func activate(_ application: NSRunningApplication, _ window: AXUIElement) {
    application.activate(options: [])
    _ = AXUIElementPerformAction(window, kAXRaiseAction as CFString)
    _ = AXUIElementSetAttributeValue(window, kAXMainAttribute as CFString, kCFBooleanTrue)
    _ = AXUIElementSetAttributeValue(window, kAXFocusedAttribute as CFString, kCFBooleanTrue)
    Thread.sleep(forTimeInterval: 0.15)
}
do {
    guard CommandLine.arguments.count == 2 else { throw ProbeFailure.invalidInput }
    let input = try JSONDecoder().decode(Input.self, from: Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1])))
    guard let app = NSRunningApplication(processIdentifier: input.pid), !app.isTerminated,
          let native = ownedWindow(input.pid), let bounds = native[kCGWindowBounds as String] as? [String: Any] else { throw ProbeFailure.missingWindow }
    let application = AXUIElementCreateApplication(input.pid)
    switch input.command {
    case .info:
        let data = try JSONSerialization.data(withJSONObject: ["pid": input.pid, "window_id": native[kCGWindowNumber as String] ?? 0, "bounds": bounds, "accessibility_trusted": AXIsProcessTrusted(), "post_events_allowed": CGPreflightPostEventAccess()], options: [.sortedKeys])
        print(String(decoding: data, as: UTF8.self))
    case .click, .close:
        guard AXIsProcessTrusted(), let windows = attribute(application, kAXWindowsAttribute as CFString) as? [AXUIElement], let window = windows.first else { throw ProbeFailure.accessibilityUnavailable }
        activate(app, window)
        guard NSWorkspace.shared.frontmostApplication?.processIdentifier == input.pid else { throw ProbeFailure.missingWindow }
        switch input.command {
        case .info: throw ProbeFailure.invalidInput
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

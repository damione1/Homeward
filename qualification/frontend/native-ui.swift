import Foundation
import AppKit
import ApplicationServices
import CoreGraphics

// Qualification only: address one owned process/window, never enumerate other
// applications, inspect editor values, or retain credentials in input files.
enum Command: String, Decodable {
    case info, click, close, minimize, restore, focus
    case tap, resize, key, scroll, tree, press, release, gesture
    case gestureBegin = "gesture-begin"
    case canvas
    case fillLogin = "fill-login", read, signOut = "sign-out", scrollEnd = "scroll-end", checkRow = "check-row"
}
enum NativeGesture: String, Decodable {
    case tap, hold, doubleTap = "double-tap"
}
enum CanvasGesture: String, Decodable { case inspect, select }
enum NativeKey: String, Decodable {
    case tab, backtab, up, down, home, end, enter, escape, space
    case left, right, shiftLeft = "shift-left", shiftRight = "shift-right"
    case letterK = "letter-k", letterI = "letter-i", letterT = "letter-t", letterC = "letter-c"
    case letterH = "letter-h", letterE = "letter-e", letterN = "letter-n"
    case digit0 = "digit-0", digit1 = "digit-1", digit2 = "digit-2", digit5 = "digit-5"
    case digit3 = "digit-3", digit4 = "digit-4", digit6 = "digit-6", digit7 = "digit-7", digit8 = "digit-8", digit9 = "digit-9"
    case minus, period, colon, civilSeparator = "civil-separator", selectAll = "select-all"
}
struct Input: Decodable {
    let command: Command; let pid: Int32; let x: Double?; let y: Double?
    let syntheticOnly: Bool?; let loginName: String?; let password: String?
    let expectedRowLabel: String?
    let label: String?; let key: NativeKey?
    let width: Double?; let height: Double?; let delta: Int32?
    let fraction: Double?
    let gesture: NativeGesture?
    let canvasGesture: CanvasGesture?; let endFraction: Double?
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
        return ["AXButton", "AXPopUpButton", "AXComboBox", "AXRow", "AXCell", "AXMenuItem", "AXRadioButton", "AXTextField", "AXStaticText"].contains(role)
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
func tap(_ window: AXUIElement, _ label: String, at requestedPoint: CGPoint? = nil) throws {
    guard let target = action(window, label) else { throw ProbeFailure.invalidInput }
    let frame = try geometry(target), bounds = try geometry(window)
    let point = requestedPoint ?? CGPoint(x: frame.midX, y: frame.midY)
    let mainHeight = NSScreen.screens.first?.frame.height ?? 0
    guard frame.contains(point), bounds.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: { screen in
        CGRect(x: screen.frame.minX, y: mainHeight - screen.frame.maxY, width: screen.frame.width, height: screen.frame.height).contains(point)
    }) else { throw ProbeFailure.invalidInput }
    for kind in [CGEventType.mouseMoved, .leftMouseDown, .leftMouseUp] {
        CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.05)
    }
}
// One owned synthetic window and one fixed-duration pointer sequence. Keeping
// a double tap inside this process avoids process-launch latency changing its
// meaning. No caller-supplied script, arbitrary timing or device RPC is used.
func nativeGesture(_ window: AXUIElement, _ label: String, _ gesture: NativeGesture) throws {
    guard let target = action(window, label) else { throw ProbeFailure.invalidInput }
    let frame = try geometry(target), bounds = try geometry(window)
    let point = CGPoint(x: frame.midX, y: frame.midY)
    let mainHeight = NSScreen.screens.first?.frame.height ?? 0
    guard bounds.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: { screen in
        CGRect(x: screen.frame.minX, y: mainHeight - screen.frame.maxY, width: screen.frame.width, height: screen.frame.height).contains(point)
    }) else { throw ProbeFailure.invalidInput }
    func post(_ kind: CGEventType, _ count: Int64) {
        let event = CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)
        event?.setIntegerValueField(.mouseEventClickState, value: count)
        event?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.04)
    }
    post(.mouseMoved, 1)
    switch gesture {
    case .tap:
        post(.leftMouseDown, 1); post(.leftMouseUp, 1)
    case .hold:
        post(.leftMouseDown, 1)
        Thread.sleep(forTimeInterval: 0.7)
        post(.leftMouseUp, 1)
    case .doubleTap:
        post(.leftMouseDown, 1); post(.leftMouseUp, 1)
        Thread.sleep(forTimeInterval: 0.06)
        post(.leftMouseDown, 2); post(.leftMouseUp, 2)
    }
}
// A bounded pointer sequence inside one explicitly labelled synthetic canvas.
// Fractions select positions in that AX group; neither arbitrary screen paths
// nor caller-provided event scripts/timings are admitted.
func canvasGesture(_ window: AXUIElement, _ label: String, _ gesture: CanvasGesture, _ fraction: Double, _ endFraction: Double?) throws {
    guard fraction.isFinite, fraction >= 0, fraction <= 1,
          let target = descendants(window).first(where: { item in
              attribute(item, kAXRoleAttribute as CFString) as? String == "AXGroup"
              && [kAXTitleAttribute, kAXDescriptionAttribute].contains { attribute(item, $0 as CFString) as? String == label }
          }) else { throw ProbeFailure.invalidInput }
    let frame = try geometry(target), bounds = try geometry(window)
    let visible = frame.intersection(bounds.insetBy(dx: 1, dy: 30))
    guard frame.width >= 1, !visible.isNull, visible.height >= 2 else { throw ProbeFailure.invalidInput }
    let y = visible.minY + min(40, visible.height / 4)
    let start = CGPoint(x: frame.minX + 0.5 + max(0, frame.width - 1) * fraction, y: y)
    let end: CGPoint
    switch gesture {
    case .inspect: guard endFraction == nil else { throw ProbeFailure.invalidInput }; end = start
    case .select:
        guard let endFraction, endFraction.isFinite, endFraction >= 0, endFraction <= 1 else { throw ProbeFailure.invalidInput }
        end = CGPoint(x: frame.minX + 0.5 + max(0, frame.width - 1) * endFraction, y: y)
    }
    let mainHeight = NSScreen.screens.first?.frame.height ?? 0
    for point in [start, end] {
        guard frame.contains(point), bounds.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: { screen in
            CGRect(x: screen.frame.minX, y: mainHeight - screen.frame.maxY, width: screen.frame.width, height: screen.frame.height).contains(point)
        }) else { throw ProbeFailure.invalidInput }
    }
    func post(_ kind: CGEventType, _ point: CGPoint) {
        CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.04)
    }
    post(.mouseMoved, start); post(.leftMouseDown, start)
    switch gesture {
    case .inspect: break
    case .select:
        for step in 1...8 { post(.leftMouseDragged, CGPoint(x: start.x + (end.x - start.x) * Double(step) / 8, y: y)) }
    }
    post(.leftMouseUp, end)
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
    case .left: code = 123; flags = []
    case .right: code = 124; flags = []
    case .shiftLeft: code = 123; flags = .maskShift
    case .shiftRight: code = 124; flags = .maskShift
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
    case .digit0: code = 29; flags = []
    case .digit1: code = 18; flags = []
    case .digit2: code = 19; flags = []
    case .digit3: code = 20; flags = []
    case .digit4: code = 21; flags = []
    case .digit5: code = 23; flags = []
    case .digit6: code = 22; flags = []
    case .digit7: code = 26; flags = []
    case .digit8: code = 28; flags = []
    case .digit9: code = 25; flags = []
    case .minus: code = 27; flags = []
    case .period: code = 47; flags = []
    case .colon: code = 41; flags = .maskShift
    case .civilSeparator: code = 17; flags = .maskShift
    case .selectAll: code = 0; flags = .maskCommand
    }
    // Synthetic numeric fixtures must type the same characters independently
    // of the user's active keyboard layout. They still travel as native key
    // events through the editor; no AX value injection or editor read is used.
    let fixtureText: String?
    switch key {
    case .digit0: fixtureText = "0"
    case .digit1: fixtureText = "1"
    case .digit2: fixtureText = "2"
    case .digit3: fixtureText = "3"
    case .digit4: fixtureText = "4"
    case .digit5: fixtureText = "5"
    case .digit6: fixtureText = "6"
    case .digit7: fixtureText = "7"
    case .digit8: fixtureText = "8"
    case .digit9: fixtureText = "9"
    case .minus: fixtureText = "-"
    case .period: fixtureText = "."
    case .colon: fixtureText = ":"
    case .civilSeparator: fixtureText = "T"
    case .selectAll: fixtureText = "a"
    case .tab, .backtab, .up, .down, .left, .right, .shiftLeft, .shiftRight, .home, .end, .enter, .escape, .space,
         .letterK, .letterI, .letterT, .letterC, .letterH, .letterE, .letterN:
        fixtureText = nil
    }
    // Winit tracks modifier transitions separately from text key events. A
    // flag on the A event alone does not reproduce pressing Command+A.
    let modifier: CGKeyCode? = flags.contains(.maskCommand) ? 55 : flags.contains(.maskShift) ? 56 : nil
    if let modifier = modifier {
        guard let event = CGEvent(keyboardEventSource: CGEventSource(stateID: .hidSystemState), virtualKey: modifier, keyDown: true) else { throw ProbeFailure.invalidInput }
        event.flags = flags; event.postToPid(pid)
        Thread.sleep(forTimeInterval: 0.06)
    }
    for down in [true, false] {
        guard let event = CGEvent(keyboardEventSource: CGEventSource(stateID: .hidSystemState), virtualKey: code, keyDown: down) else { throw ProbeFailure.invalidInput }
        event.flags = flags
        if let text = fixtureText {
            let units = Array(text.utf16)
            units.withUnsafeBufferPointer { buffer in
                event.keyboardSetUnicodeString(stringLength: buffer.count, unicodeString: buffer.baseAddress!)
            }
        }
        event.postToPid(pid)
        Thread.sleep(forTimeInterval: 0.06)
    }
    if let modifier = modifier {
        guard let event = CGEvent(keyboardEventSource: CGEventSource(stateID: .hidSystemState), virtualKey: modifier, keyDown: false) else { throw ProbeFailure.invalidInput }
        event.flags = []; event.postToPid(pid)
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
    case .click, .close, .minimize, .restore, .focus, .fillLogin, .read, .signOut, .scrollEnd, .checkRow, .tap, .resize, .key, .scroll, .tree, .press, .release, .gesture, .gestureBegin, .canvas:
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
        case .canvas:
            guard input.syntheticOnly == true, let label = input.label,
                  let gesture = input.canvasGesture, let fraction = input.fraction else { throw ProbeFailure.invalidInput }
            try canvasGesture(window, label, gesture, fraction, input.endFraction)
        case .gesture:
            guard input.syntheticOnly == true, let label = input.label, let gesture = input.gesture else { throw ProbeFailure.invalidInput }
            try nativeGesture(window, label, gesture)
        case .gestureBegin:
            guard input.syntheticOnly == true, let label = input.label,
                  let target = action(window, label) else { throw ProbeFailure.invalidInput }
            let frame = try geometry(target), bounds = try geometry(window)
            let point = CGPoint(x: frame.midX, y: frame.midY)
            let mainHeight = NSScreen.screens.first?.frame.height ?? 0
            guard bounds.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: {
                CGRect(x: $0.frame.minX, y: mainHeight - $0.frame.maxY, width: $0.frame.width, height: $0.frame.height).contains(point)
            }) else { throw ProbeFailure.invalidInput }
            for kind in [CGEventType.mouseMoved, .leftMouseDown] {
                CGEvent(mouseEventSource: nil, mouseType: kind, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
                Thread.sleep(forTimeInterval: 0.05)
            }
            let result = try JSONSerialization.data(withJSONObject: ["x": point.x, "y": point.y])
            FileHandle.standardOutput.write(result)
        case .tap:
            guard input.syntheticOnly == true, let label = input.label else { throw ProbeFailure.invalidInput }
            guard (input.x == nil) == (input.y == nil) else { throw ProbeFailure.invalidInput }
            let point: CGPoint?
            if let x = input.x, let y = input.y { point = CGPoint(x: x, y: y) } else { point = nil }
            try tap(window, label, at: point)
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
            let frame = try geometry(window)
            guard (input.x == nil) == (input.y == nil) else { throw ProbeFailure.invalidInput }
            let point = CGPoint(x: input.x ?? frame.midX, y: input.y ?? frame.midY)
            let mainHeight = NSScreen.screens.first?.frame.height ?? 0
            guard frame.insetBy(dx: 1, dy: 30).contains(point), NSScreen.screens.contains(where: { CGRect(x: $0.frame.minX, y: mainHeight - $0.frame.maxY, width: $0.frame.width, height: $0.frame.height).contains(point) }) else { throw ProbeFailure.invalidInput }
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

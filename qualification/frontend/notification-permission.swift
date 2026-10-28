// Qualification only: allow the notification prompt for one verified private
// Homeward bundle. Never grant accessibility, edit TCC, inspect arbitrary applications
// or press a dialog belonging to another app.
import Foundation
import AppKit
import ApplicationServices

enum SystemAgent: String, CaseIterable {
    case userNotifications = "com.apple.UserNotificationCenter"
    case notifications = "com.apple.notificationcenterui"
    case userNotificationsUI = "com.apple.usernotifications.usernotificationcenterui"
    case coreServices = "com.apple.CoreServicesUIAgent"
}
enum AllowLabel: String, CaseIterable { case english = "Allow", french = "Autoriser" }
enum Status: String, Codable { case pressed, noMatchingPrompt, accessibilityUnavailable, invalidOwnedApplication }
struct Input: Decodable { let pid: Int32; let bundle: String; let syntheticOnly: Bool }
struct Report: Encodable { let status: Status; let onlyOwnedNotificationPrompt: Bool }
func attribute(_ item: AXUIElement, _ name: CFString) -> CFTypeRef? {
    var value: CFTypeRef?
    return AXUIElementCopyAttributeValue(item, name, &value) == .success ? value : nil
}
func strings(_ item: AXUIElement) -> [String] {
    // Do not read editable values or an application's generic text fields.
    var values = [kAXTitleAttribute, kAXDescriptionAttribute].compactMap { attribute(item, $0 as CFString) as? String }
    if attribute(item, kAXRoleAttribute as CFString) as? String == kAXStaticTextRole as String,
       let value = attribute(item, kAXValueAttribute as CFString) as? String { values.append(value) }
    return values
}
func actions(_ item: AXUIElement) -> [String] {
    var value: CFArray?
    return AXUIElementCopyActionNames(item, &value) == .success ? value as? [String] ?? [] : []
}
func descendants(_ item: AXUIElement) -> [AXUIElement] {
    var pending = [(item, 0)], result = [AXUIElement]()
    while let (element, depth) = pending.popLast(), result.count < 512 {
        result.append(element)
        if depth < 16, let children = attribute(element, kAXChildrenAttribute as CFString) as? [AXUIElement] {
            pending.append(contentsOf: children.map { ($0, depth + 1) })
        }
    }
    return result
}
func run(_ input: Input) -> Status {
    let directory = URL(fileURLWithPath: input.bundle).standardizedFileURL.resolvingSymlinksInPath()
    let allowedRoot = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Caches/codex-build/homeward/frontend/nhf022/native-").path
    guard input.syntheticOnly, input.pid > 1, directory.path.hasPrefix(allowedRoot), directory.pathExtension == "app",
          let bundle = Bundle(url: directory), let identifier = bundle.bundleIdentifier,
          identifier.hasPrefix("ca.homeward.qualification.notifications."),
          let suffix = identifier.split(separator: ".").last, UInt64(suffix) != nil,
          let name = bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String,
          name.hasPrefix("Homeward Notification Qualification "),
          let owned = NSRunningApplication(processIdentifier: input.pid), !owned.isTerminated,
          owned.bundleURL?.standardizedFileURL.resolvingSymlinksInPath() == directory else { return .invalidOwnedApplication }
    guard AXIsProcessTrusted() else { return .accessibilityUnavailable }
    let deadline = Date().addingTimeInterval(8)
    while Date() < deadline, !owned.isTerminated {
        // Notification permission banners need not take keyboard focus. Query
        // only these exact OS agents; never enumerate unrelated applications.
        let pids = Set([input.pid] + SystemAgent.allCases.flatMap {
            NSRunningApplication.runningApplications(withBundleIdentifier: $0.rawValue).map(\.processIdentifier)
        })
        for pid in pids {
            let application = AXUIElementCreateApplication(pid)
            let windows = attribute(application, kAXWindowsAttribute as CFString) as? [AXUIElement] ?? []
            for window in windows.prefix(8) {
                // Actions can be attached directly to the permission banner,
                // rather than exposed as child buttons. Bind its own label to
                // the full unique app name before inspecting or invoking one.
                let banners = descendants(window).filter {
                    attribute($0, kAXRoleAttribute as CFString) as? String == kAXGroupRole as String
                        && strings($0).contains(where: { $0.contains(name) })
                }
                for banner in banners {
                    let allowed = actions(banner).filter { action in
                        var description: CFString?
                        _ = AXUIElementCopyActionDescription(banner, action as CFString, &description)
                        if let label = description as String?, AllowLabel(rawValue: label) != nil { return true }
                        // macOS exposes custom banner actions with an opaque
                        // action name whose first line is Name:<localized label>.
                        // Pass the original enumerated action back unchanged.
                        let firstLine = action.split(separator: "\n", maxSplits: 1).first.map(String.init) ?? ""
                        return AllowLabel.allCases.contains(where: { firstLine == "Name:" + $0.rawValue })
                    }
                    if allowed.count == 1,
                       AXUIElementPerformAction(banner, allowed[0] as CFString) == .success { return .pressed }
                    // Keep support for systems exposing an ordinary button.
                    let buttons = descendants(banner).filter { item in
                        attribute(item, kAXRoleAttribute as CFString) as? String == kAXButtonRole as String
                            && strings(item).contains(where: { AllowLabel(rawValue: $0) != nil })
                    }
                    if buttons.count == 1,
                       AXUIElementPerformAction(buttons[0], kAXPressAction as CFString) == .success { return .pressed }
                }
            }
        }
        Thread.sleep(forTimeInterval: 0.02)
    }
    return .noMatchingPrompt
}
let data = FileHandle.standardInput.readDataToEndOfFile()
let input = try JSONDecoder().decode(Input.self, from: data)
let output = Report(status: run(input), onlyOwnedNotificationPrompt: true)
FileHandle.standardOutput.write(try JSONEncoder().encode(output))

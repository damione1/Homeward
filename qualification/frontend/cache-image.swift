import Foundation
import Vision

// Read only an owned qualification capture. Emit booleans, never recognized
// text, editor contents, endpoint details or fixture credentials.
enum Expectation: String { case ready, cleared }
enum Failure: Error { case input }
do {
    guard CommandLine.arguments.count == 3,
          let expectation = Expectation(rawValue: CommandLine.arguments[2]) else { throw Failure.input }
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = .accurate
    request.recognitionLanguages = ["fr-FR", "en-US"]
    request.usesLanguageCorrection = false
    try VNImageRequestHandler(url: URL(fileURLWithPath: CommandLine.arguments[1])).perform([request])
    let lines = (request.results ?? []).compactMap { $0.topCandidates(1).first?.string }
    let combined = lines.joined(separator: " ").folding(options: [.diacriticInsensitive, .caseInsensitive], locale: Locale(identifier: "fr_FR"))
    let loading = combined.contains("chargement")
    let total = combined.contains("850") && combined.contains("entite")
    let valid: Bool
    switch expectation {
    case .ready: valid = total && !loading
    case .cleared: valid = !total && !loading
    }
    let report: [String: Any] = ["expectation": expectation.rawValue, "status": valid ? "PASS" : "FAIL", "recognized_line_count": lines.count, "contains_loading_placeholder": loading, "contains_850_entity_status": total]
    let data = try JSONSerialization.data(withJSONObject: report, options: [.sortedKeys])
    print(String(decoding: data, as: UTF8.self))
    exit(valid ? 0 : 1)
} catch {
    fputs("Owned cache image inspection failed.\n", stderr)
    exit(2)
}

import Foundation
import Vision
import ImageIO

// Qualification screenshots only. Returns observed labels and their pixel
// bounds so the harness can address the owned Android surface by real touch.
guard CommandLine.arguments.count == 2,
      let image = CGImageSourceCreateWithURL(URL(fileURLWithPath: CommandLine.arguments[1]) as CFURL, nil),
      let bitmap = CGImageSourceCreateImageAtIndex(image, 0, nil) else { exit(1) }
do {
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = .accurate
    request.recognitionLanguages = ["fr-FR", "en-US"]
    request.usesLanguageCorrection = false
    try VNImageRequestHandler(cgImage: bitmap).perform([request])
    let width = Double(bitmap.width), height = Double(bitmap.height)
    let labels: [[String: Any]] = (request.results ?? []).compactMap { observation in
        guard let candidate = observation.topCandidates(1).first else { return nil }
        let box = observation.boundingBox
        return ["label": candidate.string, "confidence": candidate.confidence,
                "x": box.minX * width, "y": (1 - box.maxY) * height,
                "width": box.width * width, "height": box.height * height]
    }
    print(String(decoding: try JSONSerialization.data(withJSONObject: labels, options: [.sortedKeys]), as: UTF8.self))
} catch { exit(1) }

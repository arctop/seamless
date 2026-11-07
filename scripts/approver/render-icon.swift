// Render the Touch ID approver's app icon, macos/SeamApprove/Bundle/AppIcon.icns,
// from the Seamless mark. Run it from the repository root when the mark or the
// plate changes, and commit the .icns it writes:
//
//   swift scripts/approver/render-icon.swift macos/SeamApprove/Bundle/AppIcon.icns
//
// It draws the sizes of an .iconset with CoreGraphics into a temporary
// directory and packs them with `iconutil -c icns`. The build never runs it:
// scripts/approver/build.sh copies the committed .icns, so `make approver` and
// CI need no renderer (stock macOS has none that renders SVG faithfully).
//
// The iconset has no 1x 16 or 32 point image (icon_16x16.png, icon_32x32.png),
// on purpose. macOS 27 draws an icon whose .icns carries either of them on a
// gray backing plate at 16 to 32 points 1x, whatever the image (Calculator's
// own 32 px rendering put there was plated too); without them it scales the
// @2x images down and plates nothing. A missing 1x image is the ordinary .icns
// case, which earlier macOS also fills from the @2x one (not measured here).
//
// The mark is docs/static/favicon.svg's: on its 32-unit tile, an empty set (a
// circle of radius 8.1 at the centre, crossed by a stroke from (6.8,25.2) to
// (24.1,7.9) with round caps) and a gold star at (25,7). Not the console's
// favicon, internal/console/static/favicon.svg, an older mark. The geometry is
// laid unchanged onto the macOS app icon grid, the 32-unit tile mapped onto the
// plate:
//
//   canvas  1024; plate 824 x 824 centred (inset 100), continuous ("squircle")
//           corners of radius 185.4. Smaller sizes inset floor(points * 100 /
//           1024) points, the mask macOS 27 itself draws each size with (16
//           points: a 14 point plate at 1), so it uses each image unscaled
//   shadow  black at 20%, 12 down, blur 40 (per 1024), matched to the drop
//           shadow measured in Calculator's AppIcon.icns
//   rim     an inner hairline of white at 14%, so the dark plate keeps its
//           edge on a dark background
//
// Only the weights change with the size, the way the favicon is the site's
// #i-mark (docs/index.html) with heavier strokes so it holds at 16 px:
//
//   64 px and up  the #i-mark's weights at the favicon's 0.94 scale: stroke
//                 1.786, star radius 2.444
//   32 px         (16 points @2x) the favicon's: stroke 2.6, star radius 3
//
// Colours are the favicon's: plate #0b1020, mark #f2f1f8, star #f3c46b (sRGB).

import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data("render-icon: \(message)\n".utf8))
    exit(1)
}

guard CommandLine.arguments.count == 2, CommandLine.arguments[1].hasSuffix(".icns") else {
    fail("usage: swift scripts/approver/render-icon.swift <out>.icns")
}
let output = URL(fileURLWithPath: CommandLine.arguments[1])

let srgb = CGColorSpace(name: CGColorSpace.sRGB)!

func color(_ hex: UInt32, alpha: CGFloat = 1) -> CGColor {
    let c = { (shift: UInt32) in CGFloat((hex >> shift) & 0xff) / 255 }
    return CGColor(colorSpace: srgb, components: [c(16), c(8), c(0), alpha])!
}

let plateColor = color(0x0b1020)
let markColor = color(0xf2f1f8)
let starColor = color(0xf3c46b)

// A rounded rectangle with continuous corners, the shape of a macOS app icon:
// each corner is three cubic curves that ease from the straight edge into the
// arc (1.528 radii from the corner) instead of meeting it at a tangent. These
// are the widely used control points that reproduce UIKit's continuous corner;
// at 256 px the edge runs within two pixels of Calculator's plate (five on the
// first row, where the curve is nearly tangent to the edge).
func continuousRoundedRect(_ rect: CGRect, radius r: CGFloat) -> CGPath {
    // Clockwise in a y-down space: each corner's point k, the unit vector a
    // back along the edge that arrives at it, and b along the edge that leaves.
    let corners: [(k: CGPoint, a: CGVector, b: CGVector)] = [
        (CGPoint(x: rect.maxX, y: rect.minY), CGVector(dx: -1, dy: 0), CGVector(dx: 0, dy: 1)),
        (CGPoint(x: rect.maxX, y: rect.maxY), CGVector(dx: 0, dy: -1), CGVector(dx: -1, dy: 0)),
        (CGPoint(x: rect.minX, y: rect.maxY), CGVector(dx: 1, dy: 0), CGVector(dx: 0, dy: -1)),
        (CGPoint(x: rect.minX, y: rect.minY), CGVector(dx: 0, dy: 1), CGVector(dx: 1, dy: 0)),
    ]
    let path = CGMutablePath()
    path.move(to: CGPoint(x: rect.minX + 1.52866471 * r, y: rect.minY))
    for c in corners {
        func p(_ s: CGFloat, _ t: CGFloat) -> CGPoint {
            CGPoint(x: c.k.x + (c.a.dx * s + c.b.dx * t) * r, y: c.k.y + (c.a.dy * s + c.b.dy * t) * r)
        }
        path.addLine(to: p(1.52866471, 0))
        path.addCurve(to: p(0.66993427, 0.06549600), control1: p(1.08849323, 0), control2: p(0.86840689, 0))
        path.addCurve(to: p(0.06549600, 0.66993427), control1: p(0.37282900, 0.18381058), control2: p(0.18381058, 0.37282900))
        path.addCurve(to: p(0, 1.52866471), control1: p(0, 0.86840689), control2: p(0, 1.08849323))
    }
    path.closeSubpath()
    return path
}

func render(points: Int, scale: Int) -> CGImage {
    let pixels = points * scale
    let size = CGFloat(pixels)
    let k = size / 1024 // canvas units per 1024
    guard let ctx = CGContext(data: nil, width: pixels, height: pixels, bitsPerComponent: 8, bytesPerRow: 0,
                              space: srgb, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
    else { fail("cannot create a \(pixels) px bitmap") }
    // y-down, so the favicon's SVG coordinates read as they are written. A
    // shadow offset is in device space and ignores this flip.
    ctx.translateBy(x: 0, y: size)
    ctx.scaleBy(x: 1, y: -1)

    let inset = CGFloat(points * 100 / 1024 * scale) // whole points, rounded down
    let plate = CGRect(x: inset, y: inset, width: size - 2 * inset, height: size - 2 * inset)
    let shape = continuousRoundedRect(plate, radius: plate.width * 185.4 / 824)

    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: -12 * k), blur: 40 * k, color: color(0x000000, alpha: 0.2))
    ctx.addPath(shape)
    ctx.setFillColor(plateColor)
    ctx.fillPath()
    ctx.restoreGState()

    // The rim: a stroke centred on the edge, clipped to the plate, so half of
    // it (at least one pixel) lies inside.
    ctx.saveGState()
    ctx.addPath(shape)
    ctx.clip()
    ctx.addPath(shape)
    ctx.setLineWidth(2 * max(1, 4 * k))
    ctx.setStrokeColor(color(0xffffff, alpha: 0.14))
    ctx.strokePath()
    ctx.restoreGState()

    // The mark, in the favicon's 32-unit tile coordinates.
    let u = plate.width / 32
    func at(_ x: CGFloat, _ y: CGFloat) -> CGPoint { CGPoint(x: plate.minX + x * u, y: plate.minY + y * u) }
    let (stroke, star): (CGFloat, CGFloat) = pixels <= 32 ? (2.6, 3) : (1.786, 2.444)

    ctx.setStrokeColor(markColor)
    ctx.setLineWidth(stroke * u)
    ctx.setLineCap(.round)
    let ring = 8.1 * u
    let centre = at(16, 16)
    ctx.strokeEllipse(in: CGRect(x: centre.x - ring, y: centre.y - ring, width: 2 * ring, height: 2 * ring))
    ctx.move(to: at(6.8, 25.2))
    ctx.addLine(to: at(24.1, 7.9))
    ctx.strokePath()

    let dot = star * u
    let starCentre = at(25, 7)
    ctx.setFillColor(starColor)
    ctx.fillEllipse(in: CGRect(x: starCentre.x - dot, y: starCentre.y - dot, width: 2 * dot, height: 2 * dot))

    guard let image = ctx.makeImage() else { fail("cannot render \(pixels) px") }
    return image
}

let fm = FileManager.default
let work = fm.temporaryDirectory.appendingPathComponent("seam-render-icon-\(getpid())")
let iconset = work.appendingPathComponent("AppIcon.iconset")
defer { try? fm.removeItem(at: work) }
do {
    try fm.createDirectory(at: iconset, withIntermediateDirectories: true)
} catch {
    fail("cannot create \(iconset.path): \(error)")
}

// iconutil's names: icon_<points>x<points>[@2x].png. No 1x at 16 and 32
// points (the header says why).
for points in [16, 32, 128, 256, 512] {
    for scale in points <= 32 ? [2] : [1, 2] {
        let name = "icon_\(points)x\(points)\(scale == 2 ? "@2x" : "").png"
        let url = iconset.appendingPathComponent(name)
        guard let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil)
        else { fail("cannot write \(name)") }
        let dpi = 72 * scale
        CGImageDestinationAddImage(dest, render(points: points, scale: scale), [
            kCGImagePropertyDPIWidth: dpi, kCGImagePropertyDPIHeight: dpi,
        ] as CFDictionary)
        guard CGImageDestinationFinalize(dest) else { fail("cannot write \(name)") }
    }
}

let iconutil = Process()
iconutil.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
iconutil.arguments = ["-c", "icns", "-o", output.path, iconset.path]
do {
    try iconutil.run()
} catch {
    fail("cannot run iconutil: \(error)")
}
iconutil.waitUntilExit()
guard iconutil.terminationStatus == 0 else { fail("iconutil exited with status \(iconutil.terminationStatus)") }
print("render-icon: wrote \(output.path)")

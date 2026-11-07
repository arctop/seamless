// swift-tools-version:5.9

// Seamless Approve: the macOS Touch ID approver for locked projects (plan step
// T). A small app bundle, separate from the Go binaries (which stay CGO-free),
// built by scripts/approver/build.sh (`make approver`) and tested with
// `make approver-test`.
//
// SeamApproveCore is everything but the windows: arguments, the byte-exact
// messages the daemon verifies, the Touch ID reason, the HTTP-over-Unix-socket
// client with its peer checks, the three flows (approve, enroll, forget) over
// a key store and a UI they are handed, and the Secure Enclave key store
// itself. Its tests run without a signed bundle: they use a software key, and
// the real store only as an unsigned process sees it (no access group, so it
// refuses before any keychain call and nothing prompts). The SeamApprove
// executable adds the AppKit dialogs and main.
//
// macOS 12 is the floor because it is the Go binaries' floor (Go 1.25 and
// later require macOS 12 Monterey); every API used here is older
// (kSecUseDataProtectionKeychain is macOS 10.15, LAContext.localizedReason
// 10.13).

import PackageDescription

let package = Package(
    name: "SeamApprove",
    platforms: [.macOS(.v12)],
    products: [
        .executable(name: "SeamApprove", targets: ["SeamApprove"]),
    ],
    targets: [
        .target(name: "SeamApproveCore"),
        .executableTarget(name: "SeamApprove", dependencies: ["SeamApproveCore"]),
        .testTarget(name: "SeamApproveCoreTests", dependencies: ["SeamApproveCore"]),
    ]
)

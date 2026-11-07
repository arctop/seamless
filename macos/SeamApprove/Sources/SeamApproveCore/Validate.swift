import Foundation

/// The shapes of values that cross into the helper: from argv (the mcp-proxy
/// bridge or `seam touchid`) and from the daemon's JSON. They mirror the Go
/// side's patterns (cmd/seam/approver_app.go, cmd/seam/approve.go).
public enum Validate {
    /// Crockford's base32, the alphabet of a ULID and of an enrollment code.
    static let crockford = Set("0123456789ABCDEFGHJKMNPQRSTVWXYZ".unicodeScalars)

    /// The longest socket path a sockaddr_un holds on macOS: sun_path is 104
    /// bytes, and the last one is the terminating NUL.
    public static let maxSocketPathBytes = 103

    /// A request id: a 26-symbol ULID in upper case.
    public static func isRequestID(_ s: String) -> Bool {
        s.unicodeScalars.count == 26 && s.unicodeScalars.allSatisfy { crockford.contains($0) }
    }

    /// A duration word as the daemon offers them ("session", "1h", "4h"):
    /// 1 to 16 ASCII letters and digits, so it can never be read as a flag.
    public static func isDuration(_ s: String) -> Bool {
        (1...16).contains(s.utf8.count) && s.utf8.allSatisfy(isAlnum)
    }

    /// An approver credential id as the daemon returns it at enrollment (a
    /// ULID today): 1 to 64 ASCII letters, digits, '-' or '_'.
    public static func isCredentialID(_ s: String) -> Bool {
        (1...64).contains(s.utf8.count) && s.utf8.allSatisfy { isAlnum($0) || $0 == UInt8(ascii: "-") || $0 == UInt8(ascii: "_") }
    }

    /// The daemon's account as sudo takes it: a name, or #uid.
    /// Mirrors accountName in cmd/seam/approve.go.
    public static func isAccountName(_ s: String) -> Bool {
        var bytes = Array(s.utf8)
        if bytes.first == UInt8(ascii: "#") {
            bytes.removeFirst()
        }
        guard let first = bytes.first, bytes.count <= 255 else { return false }
        let tail: (UInt8) -> Bool = { isAlnum($0) || $0 == UInt8(ascii: "_") || $0 == UInt8(ascii: ".") || $0 == UInt8(ascii: "-") }
        return (isAlnum(first) || first == UInt8(ascii: "_")) && bytes.dropFirst().allSatisfy(tail)
    }

    /// The facts digest: lower-case hex (a SHA-256 today).
    public static func isHexDigest(_ s: String) -> Bool {
        (1...128).contains(s.utf8.count) && s.utf8.allSatisfy { (0x30...0x39).contains($0) || (0x61...0x66).contains($0) }
    }

    /// socketPathProblem says why `s` cannot be the peer_auth socket's path,
    /// or nil when it can be.
    public static func socketPathProblem(_ s: String) -> String? {
        if !s.hasPrefix("/") {
            return "the socket path \(Text.quoted(s)) is not absolute"
        }
        if s.utf8.contains(0) {
            return "the socket path contains a NUL byte"
        }
        if s.utf8.count > maxSocketPathBytes {
            return "the socket path is \(s.utf8.count) bytes long; a Unix socket path holds at most \(maxSocketPathBytes)"
        }
        return nil
    }

    static func isAlnum(_ b: UInt8) -> Bool {
        (0x30...0x39).contains(b) || (0x41...0x5A).contains(b) || (0x61...0x7A).contains(b)
    }
}

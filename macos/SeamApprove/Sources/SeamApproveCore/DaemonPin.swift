import Darwin
import Foundation

/// `DaemonPin` is the daemon's account as root pins it: the one account
/// Seamless Approve talks to when the pin is there (PeerGuard). It mirrors
/// internal/access/daemonpin.go, which seam and serve read, byte for byte in
/// what it accepts.
///
/// The file holds one line, the account's name and its uid separated by one
/// space, with at most one newline after it (`_seamless 480`). The name is a
/// letter or "_" then letters, digits, ".", "_" or "-", at most 64 bytes, and
/// is for messages; the uid is decimal with no leading zero, at least 1 and
/// within a uid's range, and is what is compared. Anything else is refused.
///
/// The pin is believed only when nobody but root can have written it: a
/// regular file owned by root and writable by no group or other account, and
/// every directory above it (on its real path, and on the path its directory
/// resolves to) owned by root and writable by no group or other account. A pin
/// that is there but fails this, or does not parse, is a `PinError`, on which
/// the guard refuses every daemon. Only a pin that is not there at all falls
/// back to the system-account rule.
public struct DaemonPin: Equatable {
    /// Where root pins the daemon's account (access.DaemonAccountPath).
    public static let path = "/opt/seamless/etc/daemon-account"
    /// The most of a pin that is read: one short line.
    public static let maxBytes = 256

    public let name: String
    public let uid: uid_t
    /// The file it was read from.
    public let path: String

    public init(name: String, uid: uid_t, path: String) {
        self.name = name
        self.uid = uid
        self.path = path
    }

    /// `load` reads the pin at `path`: nil when nothing is there, a
    /// `PinError` when something is there that is not believed.
    public static func load(path: String = DaemonPin.path, fs: PinFileSystem = .host) throws -> DaemonPin? {
        let fail = { (problem: String) in PinError(path: path, problem: problem) }
        guard try lstat(path, fs, fail) != nil else { return nil }
        let real = try resolve(path, fs, fail)
        let parent = try resolve((path as NSString).deletingLastPathComponent, fs, fail)
        var dirs = [parent]
        let realParent = (real as NSString).deletingLastPathComponent
        if realParent != parent {
            dirs.append(realParent)
        }
        for dir in dirs {
            if let problem = dirProblem(dir, fs) {
                throw fail(problem)
            }
        }
        guard let before = try lstat(real, fs, fail) else {
            throw fail("cannot be read: it is gone")
        }
        if let problem = fileProblem(before) {
            throw fail(problem)
        }
        let read: (PinFacts, Data)
        do {
            read = try fs.read(real, maxBytes + 1)
        } catch let e as PinFSError {
            throw fail("cannot be read: \(e.reason)")
        }
        if let problem = fileProblem(read.0) {
            throw fail(problem)
        }
        do {
            let (name, uid) = try parse(read.1)
            return DaemonPin(name: name, uid: uid, path: path)
        } catch let e as PinFSError {
            throw fail(e.reason)
        }
    }

    static func lstat(_ p: String, _ fs: PinFileSystem, _ fail: (String) -> PinError) throws -> PinFacts? {
        do {
            return try fs.lstat(p)
        } catch let e as PinFSError {
            throw fail("cannot be read: \(e.reason)")
        }
    }

    static func resolve(_ p: String, _ fs: PinFileSystem, _ fail: (String) -> PinError) throws -> String {
        do {
            return try fs.realPath(p)
        } catch let e as PinFSError {
            throw fail("cannot be resolved: \(e.reason)")
        }
    }

    /// `dirProblem` walks `dir` and every directory above it, and says why one
    /// could be rewritten by an account other than root.
    static func dirProblem(_ dir: String, _ fs: PinFileSystem) -> String? {
        var d = dir
        while true {
            let facts: PinFacts?
            do {
                facts = try fs.lstat(d)
            } catch {
                return "is under \(d), which cannot be read: \((error as? PinFSError)?.reason ?? "\(error)")"
            }
            guard let f = facts else {
                return "is under \(d), which cannot be read: it is gone"
            }
            if f.mode & S_IFMT != S_IFDIR {
                return "is under \(d), which is not a directory"
            }
            if f.uid != 0 {
                return "is under \(d), which is owned by uid \(f.uid): every directory above the pin must belong to root"
            }
            if f.mode & (S_IWGRP | S_IWOTH) != 0 {
                return "is under \(d), which is mode \(octal(f.mode)): every directory above the pin must be writable by root alone"
            }
            let up = (d as NSString).deletingLastPathComponent
            if up == d || d == "/" || up.isEmpty {
                return nil
            }
            d = up
        }
    }

    /// `fileProblem` says why the pin file itself could be another account's.
    static func fileProblem(_ f: PinFacts) -> String? {
        if f.mode & S_IFMT != S_IFREG {
            return "is not a regular file"
        }
        if f.uid != 0 {
            return "is owned by uid \(f.uid): it must belong to root"
        }
        if f.mode & (S_IWGRP | S_IWOTH) != 0 {
            return "is mode \(octal(f.mode)): it must be writable by root alone (0644)"
        }
        return nil
    }

    static func octal(_ mode: mode_t) -> String {
        let s = String(mode & 0o777, radix: 8)
        return String(repeating: "0", count: max(0, 4 - s.count)) + s
    }

    static let want = ": it must be one line, the daemon's account name and uid (\"_seamless 480\")"

    /// `parse` reads the one line the type's comment describes.
    static func parse(_ data: Data) throws -> (String, uid_t) {
        var bytes = Array(data)
        if bytes.count > maxBytes {
            throw PinFSError("is longer than \(maxBytes) bytes" + want)
        }
        if bytes.last == 0x0A {
            bytes.removeLast()
        }
        if bytes.contains(0x0A) || bytes.contains(0x0D) {
            throw PinFSError("has more than one line" + want)
        }
        if bytes.isEmpty {
            throw PinFSError("is empty" + want)
        }
        let fields = bytes.split(separator: 0x20, omittingEmptySubsequences: false)
        guard fields.count == 2 else {
            throw PinFSError("has \(fields.count) space-separated fields, not 2" + want)
        }
        let name = Array(fields[0]), digits = Array(fields[1])
        guard isName(name) else {
            throw PinFSError("names \(Text.quoted(String(decoding: name, as: UTF8.self), max: 80)), which is not an account name" + want)
        }
        guard let uid = uidValue(digits) else {
            throw PinFSError("gives \(Text.quoted(String(decoding: digits, as: UTF8.self), max: 80)) as the uid, which is not a uid above 0" + want)
        }
        return (String(decoding: name, as: UTF8.self), uid)
    }

    /// `isName` is access.ValidDaemonAccountName.
    static func isName(_ b: [UInt8]) -> Bool {
        guard let first = b.first, b.count <= 64 else { return false }
        let letter = { (c: UInt8) in (0x41...0x5A).contains(c) || (0x61...0x7A).contains(c) }
        let digit = { (c: UInt8) in (0x30...0x39).contains(c) }
        guard letter(first) || first == UInt8(ascii: "_") else { return false }
        return b.dropFirst().allSatisfy { letter($0) || digit($0) || $0 == UInt8(ascii: "_") || $0 == UInt8(ascii: ".") || $0 == UInt8(ascii: "-") }
    }

    static func uidValue(_ b: [UInt8]) -> uid_t? {
        guard let first = b.first, (1...10).contains(b.count), (0x31...0x39).contains(first),
              b.allSatisfy({ (0x30...0x39).contains($0) }) else { return nil }
        var v: UInt64 = 0
        for c in b {
            v = v * 10 + UInt64(c - 0x30)
        }
        return v <= UInt64(UInt32.max) ? uid_t(v) : nil
    }
}

/// `PinError` is a pin that is there and is not believed.
public struct PinError: Error, Equatable, CustomStringConvertible {
    public let path: String
    public let problem: String

    /// "the daemon account pin <path> <problem>", as the Go side words it.
    public var description: String { "the daemon account pin \(path) \(problem)" }
}

/// `PinFSError` is a failure of the file system below the trust check.
public struct PinFSError: Error, Equatable {
    public let reason: String
    public init(_ reason: String) { self.reason = reason }
}

/// What the trust check needs of one path: its owner and st_mode (type bits
/// included).
public struct PinFacts: Equatable {
    public let uid: uid_t
    public let mode: mode_t

    public init(uid: uid_t, mode: mode_t) {
        self.uid = uid
        self.mode = mode
    }
}

/// `PinFileSystem` is how a pin is read: `.host`, or a test's stand-in.
public struct PinFileSystem {
    /// lstat(2): nil when nothing is there (ENOENT).
    public var lstat: (String) throws -> PinFacts?
    /// realpath(3).
    public var realPath: (String) throws -> String
    /// Opens a resolved path (no final symlink, no wait on a FIFO), and
    /// returns the descriptor's facts (fstat) with at most n bytes; a file
    /// that is not regular is returned unread.
    public var read: (String, Int) throws -> (PinFacts, Data)

    public init(lstat: @escaping (String) throws -> PinFacts?, realPath: @escaping (String) throws -> String,
                read: @escaping (String, Int) throws -> (PinFacts, Data)) {
        self.lstat = lstat
        self.realPath = realPath
        self.read = read
    }

    static func errnoReason() -> String { String(cString: strerror(errno)) }

    public static let host = PinFileSystem(
        lstat: { path in
            var st = stat()
            if Darwin.lstat(path, &st) != 0 {
                if errno == ENOENT { return nil }
                throw PinFSError(errnoReason())
            }
            return PinFacts(uid: st.st_uid, mode: st.st_mode)
        },
        realPath: { path in
            guard let p = Darwin.realpath(path, nil) else { throw PinFSError(errnoReason()) }
            defer { free(p) }
            return String(cString: p)
        },
        read: { path, n in
            let fd = open(path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
            guard fd >= 0 else { throw PinFSError(errnoReason()) }
            defer { close(fd) }
            var st = stat()
            guard fstat(fd, &st) == 0 else { throw PinFSError(errnoReason()) }
            let facts = PinFacts(uid: st.st_uid, mode: st.st_mode)
            guard st.st_mode & S_IFMT == S_IFREG else { return (facts, Data()) }
            var out = Data()
            var buf = [UInt8](repeating: 0, count: 512)
            while out.count < n {
                let got = Darwin.read(fd, &buf, min(buf.count, n - out.count))
                if got < 0 {
                    if errno == EINTR { continue }
                    throw PinFSError(errnoReason())
                }
                if got == 0 { break }
                out.append(contentsOf: buf[0..<got])
            }
            return (facts, out)
        }
    )
}

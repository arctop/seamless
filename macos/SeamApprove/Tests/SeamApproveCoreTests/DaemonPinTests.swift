import Darwin
import Foundation
@testable import SeamApproveCore
import XCTest

/// A pin under a temp dir, read through the host's file system with two things
/// stood in for: every path under the dir is root's unless `owners` says
/// otherwise (a test cannot chown to root), and the system's directories above
/// it are root's 0755 (the test does not control them).
final class PinRig {
    let root: String
    var owners: [String: uid_t] = [:]

    init() throws {
        let made = NSTemporaryDirectory() + "pin-" + String(UUID().uuidString.prefix(8))
        try FileManager.default.createDirectory(atPath: made, withIntermediateDirectories: true)
        root = try PinFileSystem.host.realPath(made)
        chmod(root, 0o755)
    }

    deinit { try? FileManager.default.removeItem(atPath: root) }

    func stand(_ p: String, _ f: PinFacts) -> PinFacts {
        guard p == root || p.hasPrefix(root + "/") else {
            return PinFacts(uid: 0, mode: S_IFDIR | 0o755)
        }
        return PinFacts(uid: owners[p] ?? 0, mode: f.mode)
    }

    var fs: PinFileSystem {
        PinFileSystem(
            lstat: { p in try PinFileSystem.host.lstat(p).map { self.stand(p, $0) } },
            realPath: PinFileSystem.host.realPath,
            read: { p, n in
                let (f, data) = try PinFileSystem.host.read(p, n)
                return (self.stand(p, f), data)
            }
        )
    }

    @discardableResult
    func write(_ rel: String, _ body: String, mode: mode_t = 0o644) throws -> String {
        let p = root + "/" + rel
        try FileManager.default.createDirectory(atPath: (p as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
        try Data(body.utf8).write(to: URL(fileURLWithPath: p))
        chmod(p, mode)
        return p
    }

    func load(_ p: String) throws -> DaemonPin? { try DaemonPin.load(path: p, fs: fs) }
}

final class DaemonPinTests: XCTestCase {
    func testParse() throws {
        for (body, name, uid) in [("_seamless 480\n", "_seamless", uid_t(480)), ("_seamless 480", "_seamless", 480),
                                  ("seamless-daemon.v2 1", "seamless-daemon.v2", 1), ("daemon 4294967295\n", "daemon", 4_294_967_295),
                                  (String(repeating: "a", count: 64) + " 480\n", String(repeating: "a", count: 64), 480)] {
            let got = try DaemonPin.parse(Data(body.utf8))
            XCTAssertEqual(got.0, name, body)
            XCTAssertEqual(got.1, uid, body)
        }
        let bad: [(String, String)] = [
            ("", "is empty"), ("\n", "is empty"),
            ("_seamless", "has 1 space-separated fields, not 2"),
            ("_seamless 480 extra\n", "has 3 space-separated fields, not 2"),
            ("_seamless 480 \n", "has 3 space-separated fields, not 2"),
            ("_seamless  480\n", "has 3 space-separated fields, not 2"),
            ("_seamless\t480\n", "has 1 space-separated fields, not 2"),
            ("_seamless 480\r\n", "has more than one line"),
            ("_seamless 480\n\n", "has more than one line"),
            ("_seamless 480\nroot 0\n", "has more than one line"),
            ("_seamless 0\n", "as the uid, which is not a uid above 0"),
            ("_seamless 0480\n", "as the uid"),
            ("_seamless -1\n", "as the uid"),
            ("_seamless 4294967296\n", "as the uid"),
            ("_seamless 0x1e0\n", "as the uid"),
            ("#480 480\n", "which is not an account name"),
            ("1seamless 480\n", "which is not an account name"),
            (String(repeating: "a", count: 65) + " 1", "which is not an account name"),
            (String(repeating: "x", count: 257), "is longer than 256 bytes"),
        ]
        for (body, want) in bad {
            XCTAssertThrowsError(try DaemonPin.parse(Data(body.utf8)), body) { e in
                guard let e = e as? PinFSError else { return XCTFail("\(e)") }
                XCTAssertTrue(e.reason.contains(want), "\(body): \(e.reason)")
                XCTAssertTrue(e.reason.hasSuffix(": it must be one line, the daemon's account name and uid (\"_seamless 480\")"), e.reason)
            }
        }
    }

    func testTrustedAndAbsent() throws {
        let r = try PinRig()
        let p = try r.write("etc/daemon-account", "_seamless 480\n")
        XCTAssertEqual(try r.load(p), DaemonPin(name: "_seamless", uid: 480, path: p))
        XCTAssertNil(try r.load(r.root + "/daemon-account"), "nothing there: the fallback")
        XCTAssertNil(try r.load(r.root + "/no/such/dir/daemon-account"))

        // A symlink in a trusted directory to a file in another trusted one.
        let target = try r.write("real/daemon-account", "_seamless 480\n")
        let link = r.root + "/etc/link"
        XCTAssertEqual(symlink(target, link), 0)
        XCTAssertEqual(try r.load(link)?.path, link)
    }

    func testUntrusted() throws {
        let cases: [(String, (PinRig) throws -> String, String)] = [
            ("owned by another account", { r in
                let p = try r.write("etc/daemon-account", "_seamless 480\n")
                r.owners[p] = 501
                return p
            }, "is owned by uid 501: it must belong to root"),
            ("group-writable", { r in try r.write("etc/daemon-account", "_seamless 480\n", mode: 0o664) },
             "is mode 0664: it must be writable by root alone (0644)"),
            ("a group-writable parent", { r in
                let p = try r.write("etc/daemon-account", "_seamless 480\n")
                chmod(r.root + "/etc", 0o775)
                return p
            }, "etc, which is mode 0775: every directory above the pin must be writable by root alone"),
            ("a parent owned by another account", { r in
                let p = try r.write("etc/daemon-account", "_seamless 480\n")
                r.owners[r.root + "/etc"] = 501
                return p
            }, "etc, which is owned by uid 501: every directory above the pin must belong to root"),
            ("a symlink to a file in a group-writable directory", { r in
                let target = try r.write("loose/daemon-account", "_seamless 480\n")
                chmod(r.root + "/loose", 0o775)
                try FileManager.default.createDirectory(atPath: r.root + "/etc", withIntermediateDirectories: true)
                XCTAssertEqual(symlink(target, r.root + "/etc/daemon-account"), 0)
                return r.root + "/etc/daemon-account"
            }, "loose, which is mode 0775"),
            ("a symlink in a directory another account owns", { r in
                let target = try r.write("real/daemon-account", "_seamless 480\n")
                try FileManager.default.createDirectory(atPath: r.root + "/theirs", withIntermediateDirectories: true)
                XCTAssertEqual(symlink(target, r.root + "/theirs/daemon-account"), 0)
                r.owners[r.root + "/theirs"] = 501
                return r.root + "/theirs/daemon-account"
            }, "theirs, which is owned by uid 501"),
            ("a symlink to nothing", { r in
                try FileManager.default.createDirectory(atPath: r.root + "/etc", withIntermediateDirectories: true)
                XCTAssertEqual(symlink(r.root + "/gone", r.root + "/etc/daemon-account"), 0)
                return r.root + "/etc/daemon-account"
            }, "cannot be resolved"),
            ("a directory", { r in
                try FileManager.default.createDirectory(atPath: r.root + "/etc/daemon-account", withIntermediateDirectories: true)
                return r.root + "/etc/daemon-account"
            }, "is not a regular file"),
            ("a FIFO", { r in
                XCTAssertEqual(mkfifo(r.root + "/daemon-account", 0o644), 0)
                return r.root + "/daemon-account"
            }, "is not a regular file"),
            ("does not parse", { r in try r.write("etc/daemon-account", "_seamless 480 extra\n") }, "has 3 space-separated fields, not 2"),
        ]
        for (name, setup, want) in cases {
            let r = try PinRig()
            let p = try setup(r)
            XCTAssertThrowsError(try r.load(p), name) { e in
                guard let e = e as? PinError else { return XCTFail("\(name): \(e)") }
                XCTAssertEqual(e.path, p, name)
                XCTAssertTrue(e.description.hasPrefix("the daemon account pin \(p) "), e.description)
                XCTAssertTrue(e.problem.contains(want), "\(name): \(e.problem)")
            }
        }
    }

    /// The production default reads the real path; on a Mac without one it is
    /// the fallback, and either way it never throws a non-PinError.
    func testTheRealPathIsTheGoSidesPath() {
        XCTAssertEqual(DaemonPin.path, "/opt/seamless/etc/daemon-account")
        do {
            _ = try DaemonPin.load()
        } catch is PinError {
        } catch {
            XCTFail("\(error)")
        }
    }
}

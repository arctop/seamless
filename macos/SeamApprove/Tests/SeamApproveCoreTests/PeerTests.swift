import Darwin
import Foundation
@testable import SeamApproveCore
import XCTest

/// A Mac with no pin: the system-account rule applies.
let noPin: () throws -> DaemonPin? = { nil }

final class PeerTests: XCTestCase {
    func testPeerGuardRefusesTheHelpersOwnUID() {
        let g = PeerGuard(ownUID: 501, daemonPin: noPin)
        XCTAssertThrowsError(try g.admit(501))
        XCTAssertNil(g.pinned)
    }

    func testPeerGuardRefusesRoot() {
        let g = PeerGuard(ownUID: 501, daemonPin: noPin)
        XCTAssertThrowsError(try g.admit(0)) { e in
            guard case ApproverError.peer(let msg) = e else { return XCTFail("\(e)") }
            XCTAssertEqual(msg, "The process serving the socket runs as root (uid 0), not as the daemon's account, so Seamless Approve will not talk to it.")
        }
        XCTAssertNil(g.pinned)
    }

    /// A restricted local account is another uid than the owner's, but an
    /// ordinary one: refused before it is pinned, so it cannot serve the facts
    /// the owner signs.
    func testPeerGuardRefusesAnAccountThatIsNotASystemAccount() throws {
        for uid: uid_t in [500, 501, 502, 1501, 4_294_967_294] {
            let g = PeerGuard(ownUID: 503, daemonPin: noPin)
            XCTAssertThrowsError(try g.admit(uid), "uid \(uid)") { e in
                guard case ApproverError.peer(let msg) = e else { return XCTFail("\(e)") }
                XCTAssertEqual(msg, "The process serving the socket runs as uid \(uid), which is not a system account (those are below uid 500), and no pin names the daemon's account. The daemon runs as a system account of its own, so Seamless Approve will not talk to it.")
            }
            XCTAssertNil(g.pinned, "uid \(uid) is not pinned")
        }
        // The last uid of the range, and the guide's _seamless, are admitted.
        try PeerGuard(ownUID: 501, daemonPin: noPin).admit(499)
        let g = PeerGuard(ownUID: 501, daemonPin: noPin)
        try g.admit(480)
        XCTAssertEqual(g.pinned, 480)
        XCTAssertThrowsError(try g.admit(501), "the owner is still refused after a pin")
    }

    /// With a pin, only the pinned uid is the daemon: another role account
    /// (a restricted account at 484, another service at 470) is refused, and
    /// the system-account range is not asked.
    func testPeerGuardHoldsThePeerToThePin() throws {
        let pinned480: () throws -> DaemonPin? = { DaemonPin(name: "_seamless", uid: 480, path: DaemonPin.path) }
        for uid: uid_t in [484, 470, 499, 1501] {
            let g = PeerGuard(ownUID: 501, daemonPin: pinned480)
            XCTAssertThrowsError(try g.admit(uid), "uid \(uid)") { e in
                guard case ApproverError.peer(let msg) = e else { return XCTFail("\(e)") }
                XCTAssertEqual(msg, "The process serving the socket runs as uid \(uid), but /opt/seamless/etc/daemon-account pins the daemon's account as _seamless (uid 480), so Seamless Approve will not talk to it.")
            }
            XCTAssertNil(g.pinned)
        }
        let g = PeerGuard(ownUID: 501, daemonPin: pinned480)
        try g.admit(480)
        XCTAssertEqual(g.pinned, 480)

        // The pin decides, not the range: a pinned ordinary uid is admitted.
        try PeerGuard(ownUID: 501, daemonPin: { DaemonPin(name: "seamless", uid: 1501, path: DaemonPin.path) }).admit(1501)
        // The helper's own uid and root stay refused, pinned or not.
        XCTAssertThrowsError(try PeerGuard(ownUID: 501, daemonPin: { DaemonPin(name: "x", uid: 501, path: DaemonPin.path) }).admit(501))
        XCTAssertThrowsError(try PeerGuard(ownUID: 501, daemonPin: pinned480).admit(0))
    }

    /// A pin that is there but not believed refuses every peer, the would-be
    /// daemon included: it never falls back to the range.
    func testPeerGuardRefusesEveryPeerUnderABrokenPin() {
        let broken: () throws -> DaemonPin? = { throw PinError(path: DaemonPin.path, problem: "is owned by uid 501: it must belong to root") }
        for uid: uid_t in [480, 250] {
            let g = PeerGuard(ownUID: 501, daemonPin: broken)
            XCTAssertThrowsError(try g.admit(uid)) { e in
                guard case ApproverError.peer(let msg) = e else { return XCTFail("\(e)") }
                XCTAssertEqual(msg, "The daemon account pin /opt/seamless/etc/daemon-account is owned by uid 501: it must belong to root. Seamless Approve trusts no daemon until root rewrites it.")
            }
            XCTAssertNil(g.pinned)
        }
    }

    func testPeerGuardPinsTheFirstPeer() throws {
        let g = PeerGuard(ownUID: 501, daemonPin: noPin)
        try g.admit(250)
        try g.admit(250)
        XCTAssertThrowsError(try g.admit(251), "the peer may not change within one run")
        XCTAssertEqual(g.pinned, 250)
    }

    /// The real transport with the production guard, against a socket this
    /// test's own uid serves: what an agent running as the owner could set
    /// up. The guard refuses after connect, and not one byte is sent.
    func testASocketServedByThisAccountGetsNothing() throws {
        let server = try TestServer(replies: [TestServer.reply(200, "{}")])
        let t = UnixSocketTransport(path: server.path, userAgent: "t", timeoutSeconds: 5, admit: PeerGuard(ownUID: getuid(), daemonPin: noPin).admit)
        XCTAssertThrowsError(try t.exchange(HTTPRequest(method: "POST", path: "/console/approvers/touchid/enroll",
                                                        form: [FormField("code", "SECRET")]))) { e in
            guard case ApproverError.peer(let msg) = e else { return XCTFail("\(e)") }
            XCTAssertTrue(msg.contains("runs as this account"), msg)
        }
        server.stop()
        XCTAssertEqual(server.requests, [Data()], "the connection closed without a byte of the request")
    }

    func testRequireAccount() throws {
        try requireAccount("_seamless", isPeer: 250, lookup: { $0 == "_seamless" ? 250 : nil })
        XCTAssertThrowsError(try requireAccount("_seamless", isPeer: 251, lookup: { _ in 250 }))
        XCTAssertThrowsError(try requireAccount("nobody-here", isPeer: 250, lookup: { _ in nil }))
    }

    func testUIDOfAccount() {
        XCTAssertEqual(uidOfAccount("root"), 0)
        XCTAssertEqual(uidOfAccount("#250"), 250)
        XCTAssertNil(uidOfAccount("#x"))
        XCTAssertNil(uidOfAccount("no-such-account-seamless-test"))
        let me = String(cString: getpwuid(getuid())!.pointee.pw_name)
        XCTAssertEqual(uidOfAccount(me), getuid())
    }

    func testTransportErrors() {
        let t = UnixSocketTransport(path: NSTemporaryDirectory() + "sa-missing.sock", userAgent: "t", admit: { _ in })
        XCTAssertThrowsError(try t.exchange(HTTPRequest(method: "GET", path: "/"))) { e in
            guard case ApproverError.transport(let msg) = e else { return XCTFail("\(e)") }
            XCTAssertTrue(msg.contains("no daemon socket"), msg)
        }
    }
}

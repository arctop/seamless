import Darwin
import Foundation
@testable import SeamApproveCore
import XCTest

final class PeerTests: XCTestCase {
    func testPeerGuardRefusesTheHelpersOwnUID() {
        let g = PeerGuard(ownUID: 501)
        XCTAssertThrowsError(try g.admit(501))
        XCTAssertNil(g.pinned)
    }

    func testPeerGuardPinsTheFirstPeer() throws {
        let g = PeerGuard(ownUID: 501)
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
        let t = UnixSocketTransport(path: server.path, userAgent: "t", timeoutSeconds: 5, admit: PeerGuard(ownUID: getuid()).admit)
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

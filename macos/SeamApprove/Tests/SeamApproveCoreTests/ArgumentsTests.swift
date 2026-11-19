import Foundation
@testable import SeamApproveCore
import XCTest

final class ArgumentsTests: XCTestCase {
    let sock = "/opt/seamless/run/seamless.sock"
    let id = "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7"

    func testApprove() throws {
        // The exact argv cmd/seam/approver_app.go approverApproveArgs builds.
        XCTAssertEqual(try Arguments.parse(["approve", "--socket", sock, "--request", id, "--for", "session"]),
                       .approve(socket: sock, request: id, duration: "session"))
        XCTAssertEqual(try Arguments.parse(["approve", "--for=1h", "--request=\(id)", "--socket=\(sock)"]),
                       .approve(socket: sock, request: id, duration: "1h"))
        XCTAssertEqual(try Arguments.parse(["-psn_0_12345", "approve", "--socket", sock, "--request", id, "--for", "4h"]),
                       .approve(socket: sock, request: id, duration: "4h"))
    }

    func testEnrollAndForget() throws {
        XCTAssertEqual(try Arguments.parse(["enroll", "--socket", sock]), .enroll(socket: sock, name: nil))
        XCTAssertEqual(try Arguments.parse(["enroll", "--socket", sock, "--name", "  work\tMac  "]), .enroll(socket: sock, name: "work Mac"))
        XCTAssertEqual(try Arguments.parse(["forget"]), .forget)
    }

    /// seam on a client computer (role: client) adds --role client
    /// (cmd/seam/touchid.go approverEnrollArgs); without it the role is
    /// server, as an older seam's argv says nothing about it.
    func testRole() throws {
        XCTAssertEqual(try Arguments.parse(["enroll", "--socket", sock, "--name", "Touch ID", "--role", "client"]),
                       .enroll(socket: sock, name: "Touch ID", role: .client))
        XCTAssertEqual(try Arguments.parse(["enroll", "--socket", sock, "--role=server"]), .enroll(socket: sock, name: nil, role: .server))
        XCTAssertEqual(try Arguments.parse(["enroll", "--socket", sock]), .enroll(socket: sock, name: nil, role: .server))
        XCTAssertEqual(try Arguments.parse(["approve", "--socket", sock, "--request", id, "--for", "1h", "--role", "client"]),
                       .approve(socket: sock, request: id, duration: "1h", role: .client))
        XCTAssertEqual(try Arguments.parse(["approve", "--socket", sock, "--request", id, "--for", "1h"]),
                       .approve(socket: sock, request: id, duration: "1h", role: .server))
        XCTAssertThrowsError(try Arguments.parse(["enroll", "--socket", sock, "--role", "Client"]))
        XCTAssertThrowsError(try Arguments.parse(["enroll", "--socket", sock, "--role", "client", "--role", "client"]))
        XCTAssertThrowsError(try Arguments.parse(["enroll", "--socket", sock, "--role"]))
        XCTAssertThrowsError(try Arguments.parse(["forget", "--role", "client"]))
    }

    func testRefusals() {
        let bad: [[String]] = [
            [],
            ["approve"],
            ["approve", "--socket", sock, "--request", id],
            ["approve", "--socket", "relative.sock", "--request", id, "--for", "1h"],
            ["approve", "--socket", "/" + String(repeating: "a", count: 103), "--request", id, "--for", "1h"],
            ["approve", "--socket", sock, "--request", id.lowercased(), "--for", "1h"],
            ["approve", "--socket", sock, "--request", "01JC7Q2ZK5V0M8W9X3Y4Z5A6BU", "--for", "1h"],
            ["approve", "--socket", sock, "--request", id, "--for", "-1h"],
            ["approve", "--socket", sock, "--request", id, "--for", "1 hour"],
            ["approve", "--socket", sock, "--request", id, "--for", "1h", "--for", "4h"],
            ["approve", "--socket", sock, "--request", id, "--for", "1h", "extra"],
            ["approve", "--socket", sock, "--request", id, "--for"],
            ["approve", "--socket", sock, "--request", id, "--for", "1h", "--code", "ABCD"],
            ["enroll", "--socket", sock, "--code", "ABCD-EFGH-JKMN-PQRS"],
            ["enroll", "--socket", sock, "--name", "\n\t"],
            ["enroll", "--socket", sock, "--name", String(repeating: "n", count: 65)],
            ["enroll"],
            ["forget", "--socket", sock],
            ["approve-all"],
        ]
        for argv in bad {
            XCTAssertThrowsError(try Arguments.parse(argv), "\(argv)")
        }
    }

    func testSocketPathLimit() {
        XCTAssertNil(Validate.socketPathProblem("/" + String(repeating: "a", count: 102)))
        XCTAssertNotNil(Validate.socketPathProblem("/" + String(repeating: "a", count: 103)))
        XCTAssertNotNil(Validate.socketPathProblem("/a\u{0}b"))
    }

    func testValidators() {
        XCTAssertTrue(Validate.isAccountName("_seamless"))
        XCTAssertTrue(Validate.isAccountName("#250"))
        XCTAssertFalse(Validate.isAccountName("-u"))
        XCTAssertFalse(Validate.isAccountName("a b"))
        XCTAssertFalse(Validate.isAccountName(""))
        XCTAssertTrue(Validate.isHexDigest("9f86d081"))
        XCTAssertFalse(Validate.isHexDigest("9F86"))
        XCTAssertFalse(Validate.isHexDigest(""))
        XCTAssertTrue(Validate.isCredentialID("01JC7Q2ZK5V0M8W9X3Y4Z5A6B7"))
        XCTAssertFalse(Validate.isCredentialID("a b"))
    }

    func testDefaultName() {
        XCTAssertEqual(Arguments.defaultApproverName("Owner\u{2019}s MacBook Pro"), "Owner\u{2019}s MacBook Pro")
        XCTAssertEqual(Arguments.defaultApproverName("\n"), "Touch ID")
        XCTAssertLessThanOrEqual(Arguments.defaultApproverName(String(repeating: "\u{00E9}\u{0301}", count: 60)).unicodeScalars.count, 64)
    }
}

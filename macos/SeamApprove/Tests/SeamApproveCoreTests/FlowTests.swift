import Foundation
@testable import SeamApproveCore
import XCTest

final class FlowTests: XCTestCase {
    let id = "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7"
    let digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
    let challenge = Data((0..<32).map { UInt8(0xA0 + $0) })

    func accessOne(status: String = "pending", durations: String = #"["session","1h","4h"]"#,
                   account: String = "_seamless", challenge: String? = nil, requestID: String? = nil) -> String {
        let ch = challenge ?? self.challenge.base64EncodedString()
        return """
            {"request":{"id":"\(requestID ?? id)","short_id":"6B7","project":"acme","isolation":"confidential","access":"read-write",
            "kind":"request","program":"/usr/local/bin/claude","pid":4242,"uid":501,"session":"cc/2252a8cb","session_id":"01S",
            "cwd":"/Users/o/acme","repo":"acme","model":"m","reason":"please","warnings":[{"code":"no_session","text":"careful"}],
            "requested_at":"2026-11-06T10:00:00Z","expires_at":"2026-11-06T10:15:00Z","digest":"\(digest)"},
            "status":"\(status)","page":"","terminal":{"account":"\(account)","seamlessd":"/opt/seamless/bin/seamlessd","config":"/opt/seamless/seamless.yaml"},
            "durations":\(durations),"max_lease":"8h0m0s","challenge":"\(ch)"}
            """
    }

    /// A transport to `server` that admits the test's own uid, and an
    /// account lookup that names it: the stand-in daemon runs as this uid.
    func flows(_ server: TestServer, keys: KeyStore, ui: ApproverUI = FakeUI(), accountUID: uid_t = getuid()) -> Flows {
        let t = UnixSocketTransport(path: server.path, userAgent: "test", timeoutSeconds: 5, admit: { _ in })
        return Flows(transport: t, keys: keys, ui: ui, lookupAccount: { _ in accountUID }, defaultName: { "Test Mac" })
    }

    // MARK: approve

    func testApproveSignsTheDaemonsFactsAndSendsTheProof() throws {
        let keys = SoftwareKeyStore()
        let key = try keys.add(credential: "01APPROVERID000000000000AA")
        let server = try TestServer(replies: [
            TestServer.reply(200, accessOne()),
            TestServer.reply(200, #"{"approved":"6B7","project":"acme","expires_at":"2026-11-06T11:00:00Z"}"#),
        ])
        defer { server.stop() }

        let out = flows(server, keys: keys).approve(id: id, duration: "1h")
        guard case .done(let msg) = out else { return XCTFail("\(out)") }
        XCTAssertTrue(msg.hasPrefix("Approved: acme is open to session"), msg)

        let reqs = server.requestStrings
        XCTAssertEqual(reqs.count, 2)
        XCTAssertTrue(reqs[0].hasPrefix("GET /console/access/\(id)?format=json HTTP/1.0\r\n"), reqs[0])
        XCTAssertTrue(reqs[1].hasPrefix("POST /console/access/\(id)/approve?format=json HTTP/1.0\r\n"), reqs[1])
        let form = formOf(reqs[1])
        XCTAssertEqual(form["proof"], "touchid")
        XCTAssertEqual(form["credential"], "01APPROVERID000000000000AA")
        XCTAssertEqual(form["duration"], "1h")
        XCTAssertEqual(form["digest"], digest)
        let sig = try XCTUnwrap(Data(base64Encoded: form["signature"] ?? ""))
        let want = Messages.touchIDApproveMessage(id: id, challenge: challenge, digest: digest, access: "read-write", duration: "1h")
        XCTAssertTrue(keys.verifies(sig, want, handle: key.handle), "the signature covers TouchIDApproveMessage of the daemon's facts")
        XCTAssertEqual(keys.reasons, ["approve read-write access to acme (confidential) for session \u{201C}cc/2252a8cb\u{201D} (claude, pid 4242) for 1 hour. Warning: careful"])
    }

    func testApproveRefusesBeforeSigning() throws {
        let cases: [(String, String, uid_t)] = [
            (accessOne(status: "approved"), "is approved", getuid()),
            (accessOne(durations: #"["session"]"#), "does not offer the duration", getuid()),
            (accessOne(challenge: ""), "no challenge", getuid()),
            (accessOne(challenge: "!!!"), "not standard base64", getuid()),
            (accessOne(requestID: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B8"), "another request", getuid()),
            (accessOne(account: "-rf"), "not an account name", getuid()),
            (accessOne(), "is served by uid", getuid() &+ 1),
        ]
        for (json, want, accountUID) in cases {
            let keys = SoftwareKeyStore()
            try keys.add(credential: "01APPROVERID000000000000AA")
            let server = try TestServer(replies: [TestServer.reply(200, json), TestServer.reply(200, "{}")])
            let out = flows(server, keys: keys, accountUID: accountUID).approve(id: id, duration: "1h")
            server.stop()
            guard case .failed(let msg) = out else {
                XCTFail("\(want): \(out)")
                continue
            }
            XCTAssertTrue(msg.contains(want), "\(want): \(msg)")
            XCTAssertEqual(server.requests.count, 1, "\(want): nothing is posted")
            XCTAssertTrue(keys.reasons.isEmpty, "\(want): nothing is signed")
        }
    }

    func testApproveWithoutAKeyStoreOrAKeyTouchesNoSocket() throws {
        let server = try TestServer(replies: [TestServer.reply(200, accessOne())])
        defer { server.stop() }
        let unsigned = SoftwareKeyStore()
        unsigned.unavailable = "no key store"
        XCTAssertEqual(flows(server, keys: unsigned).approve(id: id, duration: "1h"), .failed("no key store"))
        guard case .failed(let msg) = flows(server, keys: SoftwareKeyStore()).approve(id: id, duration: "1h") else {
            return XCTFail("approve without an enrolled key")
        }
        XCTAssertTrue(msg.contains("seam touchid enroll"), msg)
        // Two enrolled keys: which one the daemon knows is not guessed. The
        // way out names a command seam has (a new enrolment keeps one key).
        let two = SoftwareKeyStore()
        try two.add(credential: "01APPROVERID000000000000AA")
        try two.add(credential: "01APPROVERID000000000000BB")
        guard case .failed(let several) = flows(server, keys: two).approve(id: id, duration: "1h") else {
            return XCTFail("approve with two enrolled keys")
        }
        XCTAssertTrue(several.contains("seam touchid enroll again"), several)
        XCTAssertTrue(server.requests.isEmpty)
    }

    func testApproveCancelledAtTheTouchIDPrompt() throws {
        let keys = SoftwareKeyStore()
        try keys.add(credential: "01APPROVERID000000000000AA")
        keys.cancelSigning = true
        let server = try TestServer(replies: [TestServer.reply(200, accessOne()), TestServer.reply(200, "{}")])
        let out = flows(server, keys: keys).approve(id: id, duration: "session")
        server.stop()
        XCTAssertEqual(out.exitCode, 2)
        XCTAssertEqual(server.requests.count, 1, "a cancel posts nothing")
    }

    func testApproveReportsTheDaemonsRefusal() throws {
        let keys = SoftwareKeyStore()
        try keys.add(credential: "01APPROVERID000000000000AA")
        let server = try TestServer(replies: [
            TestServer.reply(200, accessOne()),
            TestServer.reply(409, #"{"error":"this request was already decided\n"}"#),
        ])
        let out = flows(server, keys: keys).approve(id: id, duration: "4h")
        server.stop()
        XCTAssertEqual(out, .failed("The daemon refused: this request was already decided"))
    }

    // MARK: enroll

    let terminalList = #"{"requests":[],"terminal":{"account":"_seamless","seamlessd":"/opt/seamless/bin/seamlessd","config":"/opt/seamless/seamless.yaml"},"durations":["session"],"max_lease":"8h0m0s"}"#

    func testEnrollAsksForTheCodeThenSignsAndRegisters() throws {
        let keys = SoftwareKeyStore()
        let old = try keys.add(credential: "01OLDAPPROVER0000000000000")
        let ui = FakeUI(codes: ["not a code", "abcd-efgh-jkmn-pqrs"])
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(200, #"{"id":"01NEWAPPROVER0000000000000","name":"Test Mac"}"#),
        ])
        defer { server.stop() }

        let out = flows(server, keys: keys, ui: ui).enroll(name: nil)
        guard case .done(let msg) = out else { return XCTFail("\(out)") }
        XCTAssertTrue(msg.contains("01NEWAPPROVER0000000000000"), msg)
        XCTAssertEqual(ui.asked.count, 2)
        XCTAssertEqual(ui.asked[0].command, "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code touchid")
        XCTAssertNil(ui.asked[0].problem)
        XCTAssertNotNil(ui.asked[1].problem)

        let reqs = server.requestStrings
        XCTAssertEqual(reqs.count, 2)
        XCTAssertTrue(reqs[0].hasPrefix("GET /console/access?format=json HTTP/1.0\r\n"))
        XCTAssertTrue(reqs[1].hasPrefix("POST /console/approvers/touchid/enrol?format=json HTTP/1.0\r\n"))
        let form = formOf(reqs[1])
        XCTAssertEqual(form["code"], "ABCDEFGHJKMNPQRS")
        XCTAssertEqual(form["name"], "Test Mac")
        let pub = try XCTUnwrap(Data(base64Encoded: form["public_key"] ?? ""))
        XCTAssertEqual(pub.count, 65)
        XCTAssertEqual(pub.first, 0x04)

        let stored = try keys.keys()
        XCTAssertEqual(stored.count, 1, "the older key is replaced")
        XCTAssertNotEqual(stored[0].handle, old.handle)
        XCTAssertEqual(stored[0].credential, "01NEWAPPROVER0000000000000")
        XCTAssertEqual(keys.publicKey(stored[0].handle), pub)
        let sig = try XCTUnwrap(Data(base64Encoded: form["signature"] ?? ""))
        XCTAssertTrue(keys.verifies(sig, try Messages.touchIDEnrolMessage(code: "ABCDEFGHJKMNPQRS", publicKey: pub), handle: stored[0].handle))
        XCTAssertEqual(keys.reasons, [Reason.enrolment(account: "_seamless")])
    }

    func testEnrollChecksTheDaemonBeforeAskingForTheCode() throws {
        let ui = FakeUI(codes: ["abcd-efgh-jkmn-pqrs"])
        let server = try TestServer(replies: [TestServer.reply(200, terminalList), TestServer.reply(200, "{}")])
        let out = flows(server, keys: SoftwareKeyStore(), ui: ui, accountUID: getuid() &+ 1).enroll(name: nil)
        server.stop()
        guard case .failed(let msg) = out else { return XCTFail("\(out)") }
        XCTAssertTrue(msg.contains("is served by uid"), msg)
        XCTAssertTrue(ui.asked.isEmpty, "the code is never asked for when the socket is not the daemon's")
        XCTAssertEqual(server.requests.count, 1)
    }

    func testEnrollRefusedKeepsNoNewKey() throws {
        let keys = SoftwareKeyStore()
        let old = try keys.add(credential: "01OLDAPPROVER0000000000000")
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(400, #"{"error":"that enrolment code is not valid"}"#),
        ])
        let out = flows(server, keys: keys, ui: FakeUI(codes: ["ABCD-EFGH-JKMN-PQRS"])).enroll(name: "work")
        server.stop()
        XCTAssertEqual(out, .failed("The daemon refused: that enrolment code is not valid"))
        XCTAssertEqual(try keys.keys(), [old], "the new key is deleted and the enrolled one kept")
        XCTAssertEqual(formOf(server.requestStrings[1])["name"], "work")
    }

    func testEnrollCancelled() throws {
        let keys = SoftwareKeyStore()
        let server = try TestServer(replies: [TestServer.reply(200, terminalList)])
        XCTAssertEqual(flows(server, keys: keys, ui: FakeUI(codes: [nil])).enroll(name: nil).exitCode, 2)
        server.stop()

        let server2 = try TestServer(replies: [TestServer.reply(200, terminalList), TestServer.reply(200, "{}")])
        keys.cancelSigning = true
        XCTAssertEqual(flows(server2, keys: keys, ui: FakeUI(codes: ["ABCD-EFGH-JKMN-PQRS"])).enroll(name: nil).exitCode, 2)
        server2.stop()
        XCTAssertTrue(try keys.keys().isEmpty, "a key the owner did not confirm is deleted")
        XCTAssertEqual(server2.requests.count, 1)
    }

    // MARK: forget

    func testForget() throws {
        let keys = SoftwareKeyStore()
        try keys.add(credential: "01APPROVERID000000000000AA")
        let ui = FakeUI()
        ui.forget = false
        let f = Flows(transport: UnixSocketTransport(path: "/nonexistent", userAgent: "t", admit: { _ in }), keys: keys, ui: ui)
        XCTAssertEqual(f.forget().exitCode, 2)
        XCTAssertEqual(try keys.keys().count, 1)
        ui.forget = true
        guard case .done(let msg) = f.forget() else { return XCTFail() }
        XCTAssertTrue(msg.contains("01APPROVERID000000000000AA"), msg)
        XCTAssertTrue(try keys.keys().isEmpty)
        XCTAssertEqual(ui.forgetAsked, [["01APPROVERID000000000000AA"], ["01APPROVERID000000000000AA"]])
        XCTAssertEqual(f.forget().exitCode, 0, "nothing left to forget")
    }
}

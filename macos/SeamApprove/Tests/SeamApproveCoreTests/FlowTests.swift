import Foundation
import Security
@testable import SeamApproveCore
import XCTest

final class FlowTests: XCTestCase {
    let id = "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7"
    let digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
    let challenge = Data((0..<32).map { UInt8(0xA0 + $0) })

    /// 9:00 AM on Friday 2026-11-06 in New York, read in en_US.
    static let clock = WallClock(now: { ISO8601DateFormatter().date(from: "2026-11-06T14:00:00Z")! },
                                 timeZone: TimeZone(identifier: "America/New_York")!, locale: Locale(identifier: "en_US"))

    /// `agent` adds the daemon's agent name to the facts; nil leaves it out,
    /// as a daemon from before the field does.
    func accessOne(status: String = "pending", durations: String = #"["session","1h","4h"]"#,
                   account: String = "_seamless", challenge: String? = nil, requestID: String? = nil,
                   agent: String? = nil) -> String {
        let ch = challenge ?? self.challenge.base64EncodedString()
        let agentField = agent.map { #""agent":"\#($0)","# } ?? ""
        return """
            {"request":{"id":"\(requestID ?? id)","short_id":"6B7","project":"acme","isolation":"confidential","access":"read-write",
            "kind":"request","program":"/usr/local/bin/claude",\(agentField)"pid":4242,"uid":501,"session":"cc/2252a8cb","session_id":"01S",
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
        return Flows(transport: t, keys: keys, ui: ui, lookupAccount: { _ in accountUID }, defaultName: { "Test Mac" },
                     clock: FlowTests.clock)
    }

    // MARK: approve

    func testApproveSignsTheDaemonsFactsAndSendsTheProof() throws {
        let keys = SoftwareKeyStore()
        let key = try keys.add(credential: "01APPROVERID000000000000AA")
        let server = try TestServer(replies: [
            TestServer.reply(200, accessOne(agent: "Claude Code 2.1.319")),
            TestServer.reply(200, #"{"approved":"6B7","project":"acme","expires_at":"2026-11-06T19:09:00Z"}"#),
        ])
        defer { server.stop() }

        let out = flows(server, keys: keys).approve(id: id, duration: "1h")
        XCTAssertEqual(out, .done(Notice(title: "Approved",
                                         body: "Read-write access to acme is open to Claude Code 2.1.319 in session cc/\u{2060}2252a8cb until 14:09.",
                                         succeeded: true)))
        XCTAssertFalse(try XCTUnwrap(out.notice).autoCloses, "an approval stays until the owner dismisses it")

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
        XCTAssertEqual(keys.reasons, ["approve read-write access to acme (confidential) for Claude Code 2.1.319 in session cc/\u{2060}2252a8cb for 1 hour. Warning: careful"])
    }

    /// A daemon from before the agent fact: the program's name stands in,
    /// and what is signed is the same.
    func testApproveAgainstADaemonWithoutTheAgentFact() throws {
        let keys = SoftwareKeyStore()
        let key = try keys.add(credential: "01APPROVERID000000000000AA")
        let server = try TestServer(replies: [
            TestServer.reply(200, accessOne()),
            TestServer.reply(200, #"{"approved":"6B7","project":"acme","expires_at":"2026-11-07T14:15:00Z"}"#),
        ])
        defer { server.stop() }

        let out = flows(server, keys: keys).approve(id: id, duration: "4h")
        XCTAssertEqual(out.notice?.body,
                       "Read-write access to acme is open to the program \u{201C}claude\u{201D} in session cc/\u{2060}2252a8cb until tomorrow 09:15.")
        XCTAssertEqual(keys.reasons, ["approve read-write access to acme (confidential) for the program \u{201C}claude\u{201D} in session cc/\u{2060}2252a8cb for 4 hours. Warning: careful"])
        let sig = try XCTUnwrap(Data(base64Encoded: formOf(server.requestStrings[1])["signature"] ?? ""))
        let want = Messages.touchIDApproveMessage(id: id, challenge: challenge, digest: digest, access: "read-write", duration: "4h")
        XCTAssertTrue(keys.verifies(sig, want, handle: key.handle))
    }

    /// A read grant's alert names the asking project, as its prompt does.
    func testApproveAGrantNamesTheAskingProject() throws {
        let keys = SoftwareKeyStore()
        try keys.add(credential: "01APPROVERID000000000000AA")
        let grant = accessOne(agent: "Codex")
            .replacingOccurrences(of: #""kind":"request""#, with: #""kind":"grant","from":"beta""#)
            .replacingOccurrences(of: #""access":"read-write""#, with: #""access":"read-only""#)
        let server = try TestServer(replies: [
            TestServer.reply(200, grant),
            TestServer.reply(200, #"{"approved":"6B7","project":"acme","from":"beta","grant":"01G","expires_at":"2026-11-09T14:15:00Z"}"#),
        ])
        defer { server.stop() }

        let out = flows(server, keys: keys).approve(id: id, duration: "session")
        XCTAssertEqual(out.notice?.body,
                       "Read-only access to acme from project beta is open to Codex in session cc/\u{2060}2252a8cb until Mon, Nov 9, 09:15.")
        XCTAssertEqual(keys.reasons, ["approve read-only access to acme (confidential) from project beta for Codex in session cc/\u{2060}2252a8cb until the session ends (at most 8 hours). Warning: careful"])
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
            guard case .failed(let notice) = out else {
                XCTFail("\(want): \(out)")
                continue
            }
            XCTAssertEqual(notice.title, "Couldn't approve", want)
            XCTAssertTrue(notice.body.contains(want), "\(want): \(notice.body)")
            XCTAssertFalse(notice.autoCloses, "\(want): a failure stays open")
            XCTAssertEqual(server.requests.count, 1, "\(want): nothing is posted")
            XCTAssertTrue(keys.reasons.isEmpty, "\(want): nothing is signed")
        }
    }

    func testApproveWithoutAKeyStoreOrAKeyTouchesNoSocket() throws {
        let server = try TestServer(replies: [TestServer.reply(200, accessOne())])
        defer { server.stop() }
        let unsigned = SoftwareKeyStore()
        unsigned.unavailable = "no key store"
        XCTAssertEqual(flows(server, keys: unsigned).approve(id: id, duration: "1h"),
                       .failed(Notice(title: "Couldn't approve", body: "No key store.", succeeded: false)))
        guard case .failed(let none) = flows(server, keys: SoftwareKeyStore()).approve(id: id, duration: "1h") else {
            return XCTFail("approve without an enrolled key")
        }
        XCTAssertEqual(none.body, "No Touch ID approver key is enrolled on this Mac: run seam touchid enroll.")
        // Two enrolled keys: which one the daemon knows is not guessed. The
        // way out names a command seam has (a new enrollment keeps one key).
        let two = SoftwareKeyStore()
        try two.add(credential: "01APPROVERID000000000000AA")
        try two.add(credential: "01APPROVERID000000000000BB")
        guard case .failed(let several) = flows(server, keys: two).approve(id: id, duration: "1h") else {
            return XCTFail("approve with two enrolled keys")
        }
        XCTAssertTrue(several.body.contains("seam touchid enroll again"), several.body)
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
        // The bridge tells a cancel by this line's beginning, "SeamApprove:
        // Not approved:" (cmd/seam/mcpproxy_approver.go, pinned there too).
        XCTAssertEqual(out.stderrLine, "SeamApprove: Not approved: the Touch ID prompt was cancelled.")
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
        XCTAssertEqual(out, .failed(Notice(title: "Couldn't approve", body: "The daemon refused: this request was already decided.",
                                           succeeded: false)))
    }

    // MARK: enroll

    let terminalList = #"{"requests":[],"terminal":{"account":"_seamless","seamlessd":"/opt/seamless/bin/seamlessd","config":"/opt/seamless/seamless.yaml"},"durations":["session"],"max_lease":"8h0m0s"}"#

    func testEnrollAsksForTheCodeThenSignsAndRegisters() throws {
        let keys = SoftwareKeyStore()
        let old = try keys.add(credential: "01OLDAPPROVER0000000000000")
        let oldPublic = try XCTUnwrap(keys.secKey(old.handle).flatMap(SecKeyCopyPublicKey))
        let ui = FakeUI(codes: ["not a code", "abcd-efgh-jkmn-pqrs"])
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(200, #"{"id":"01NEWAPPROVER0000000000000","name":"Test Mac"}"#),
        ])
        defer { server.stop() }

        let out = flows(server, keys: keys, ui: ui).enroll(name: nil)
        // A daemon that retires nothing (one from before retirement) still
        // lists the replaced key's credential: the alert gives the command
        // that removes it, id and all, set apart, and stays open. The new
        // key's id is not shown: nothing asks the owner for it.
        XCTAssertEqual(out, .done(Notice(
            title: "Touch ID enrolled",
            body: "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as \u{201C}Test Mac\u{201D}. "
                + "The key it replaces is deleted from this Mac, but the daemon still lists that key's credential. "
                + "To remove it, run this in your own terminal:",
            setApart: ["sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers remove 01OLDAPPROVER0000000000000"],
            succeeded: true, instructs: true)))
        XCTAssertFalse(try XCTUnwrap(out.notice).autoCloses)
        XCTAssertFalse(out.message.contains("01NEWAPPROVER0000000000000"), out.message)
        XCTAssertEqual(ui.asked.count, 2)
        XCTAssertEqual(ui.asked[0].command, "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code touchid")
        XCTAssertNil(ui.asked[0].problem)
        XCTAssertEqual(ui.asked[1].problem, CodeProblem.notACode)

        let reqs = server.requestStrings
        XCTAssertEqual(reqs.count, 2)
        XCTAssertTrue(reqs[0].hasPrefix("GET /console/access?format=json HTTP/1.0\r\n"))
        XCTAssertTrue(reqs[1].hasPrefix("POST /console/approvers/touchid/enroll?format=json HTTP/1.0\r\n"))
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
        XCTAssertTrue(keys.verifies(sig, try Messages.touchIDEnrollMessage(code: "ABCDEFGHJKMNPQRS", publicKey: pub), handle: stored[0].handle))
        // The earlier key signed its own retirement, bound to this code and
        // the new key, under the same prompt.
        XCTAssertEqual(form["replace"], "01OLDAPPROVER0000000000000")
        let retireSig = try XCTUnwrap(Data(base64Encoded: form["replace_signature"] ?? ""))
        let retireMessage = try Messages.touchIDReplaceMessage(code: "ABCDEFGHJKMNPQRS", publicKey: pub, retiring: "01OLDAPPROVER0000000000000")
        XCTAssertTrue(SecKeyVerifySignature(oldPublic, .ecdsaSignatureMessageX962SHA256, retireMessage as CFData, retireSig as CFData, nil))
        XCTAssertEqual(keys.reasons, [Reason.enrollment(account: "_seamless", replacing: true)])
    }

    /// A daemon that retires the earlier key with the enrollment leaves the
    /// owner nothing to do: the alert says so and closes by itself.
    func testEnrollRetiresTheKeyItReplaces() throws {
        let keys = SoftwareKeyStore()
        try keys.add(credential: "01OLDAPPROVER0000000000000")
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(200, #"{"id":"01NEWAPPROVER0000000000000","name":"Test Mac","retired":["01OLDAPPROVER0000000000000"]}"#),
        ])
        defer { server.stop() }
        let out = flows(server, keys: keys, ui: FakeUI(codes: ["ABCD-EFGH-JKMN-PQRS"])).enroll(name: nil)
        XCTAssertEqual(out, .done(Notice(
            title: "Touch ID enrolled",
            body: "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as \u{201C}Test Mac\u{201D}. "
                + "It replaces the key this Mac enrolled before, which is deleted from this Mac and from the daemon.",
            succeeded: true)))
        XCTAssertFalse(try XCTUnwrap(out.notice).autoCloses)
        XCTAssertEqual(try keys.keys().map(\.credential), ["01NEWAPPROVER0000000000000"])
    }

    /// An earlier key whose signature is cancelled is not named to the
    /// daemon: the enrollment goes on, and the alert gives the command that
    /// removes the earlier key's credential.
    func testEnrollWhenTheEarlierKeyDoesNotSign() throws {
        let keys = SoftwareKeyStore()
        try keys.add(credential: "01OLDAPPROVER0000000000000")
        keys.cancelLater = true
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(200, #"{"id":"01NEWAPPROVER0000000000000","name":"Test Mac"}"#),
        ])
        defer { server.stop() }
        let out = flows(server, keys: keys, ui: FakeUI(codes: ["ABCD-EFGH-JKMN-PQRS"])).enroll(name: nil)
        XCTAssertEqual(out.notice?.setApart, ["sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers remove 01OLDAPPROVER0000000000000"])
        let form = formOf(server.requestStrings[1])
        XCTAssertNil(form["replace"])
        XCTAssertNil(form["replace_signature"])
        XCTAssertEqual(try keys.keys().map(\.credential), ["01NEWAPPROVER0000000000000"])
    }

    /// A first enrollment asks nothing more of the owner, and the code is
    /// taken with or without its dashes, in any case, and with the line break
    /// a paste from a terminal brings.
    func testEnrollTakesTheCodeWithOrWithoutDashes() throws {
        for typed in ["ABCDEFGHJKMNPQRS", "abcdefghjkmnpqrs", "abcd efgh jkmn pqrs", "ABCD-EFGH-JKMN-PQRS\n", " abcd-efgh-jkmn-pqrs\r\n"] {
            let keys = SoftwareKeyStore()
            let ui = FakeUI(codes: [typed])
            let server = try TestServer(replies: [
                TestServer.reply(200, terminalList),
                TestServer.reply(200, #"{"id":"01NEWAPPROVER0000000000000","name":"Test Mac"}"#),
            ])
            let out = flows(server, keys: keys, ui: ui).enroll(name: nil)
            server.stop()
            XCTAssertEqual(out, .done(Notice(title: "Touch ID enrolled",
                                             body: "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as \u{201C}Test Mac\u{201D}.",
                                             succeeded: true)), typed)
            XCTAssertFalse(try XCTUnwrap(out.notice).autoCloses, typed)
            XCTAssertEqual(ui.asked.count, 1, typed)
            XCTAssertEqual(formOf(server.requestStrings[1])["code"], "ABCDEFGHJKMNPQRS", typed)
        }
    }

    /// An older key that was never enrolled leaves nothing on the daemon to
    /// remove, so the alert gives no command.
    func testEnrollReplacingAKeyNeverEnrolledInstructsNothing() throws {
        let keys = SoftwareKeyStore()
        _ = try keys.create()
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(200, #"{"id":"01NEWAPPROVER0000000000000","name":"Test Mac"}"#),
        ])
        defer { server.stop() }
        let out = flows(server, keys: keys, ui: FakeUI(codes: ["ABCD-EFGH-JKMN-PQRS"])).enroll(name: nil)
        XCTAssertEqual(out.notice?.setApart, [])
        XCTAssertEqual(out.notice?.autoCloses, false)
        XCTAssertEqual(try keys.keys().map(\.credential), ["01NEWAPPROVER0000000000000"])
        XCTAssertNil(formOf(server.requestStrings[1])["replace"], "a key the daemon never knew has nothing to retire")
        XCTAssertEqual(keys.reasons, [Reason.enrollment(account: "_seamless")])
    }

    func testEnrollChecksTheDaemonBeforeAskingForTheCode() throws {
        let ui = FakeUI(codes: ["abcd-efgh-jkmn-pqrs"])
        let server = try TestServer(replies: [TestServer.reply(200, terminalList), TestServer.reply(200, "{}")])
        let out = flows(server, keys: SoftwareKeyStore(), ui: ui, accountUID: getuid() &+ 1).enroll(name: nil)
        server.stop()
        guard case .failed(let notice) = out else { return XCTFail("\(out)") }
        XCTAssertEqual(notice.title, "Couldn't enroll")
        XCTAssertEqual(notice.body, "The socket is served by uid \(getuid()), but the daemon names \u{201C}_seamless\u{201D} (uid \(getuid() &+ 1)) as its account, so Seamless Approve stopped.")
        XCTAssertTrue(ui.asked.isEmpty, "the code is never asked for when the socket is not the daemon's")
        XCTAssertEqual(server.requests.count, 1)
    }

    func testEnrollRefusedKeepsNoNewKey() throws {
        let keys = SoftwareKeyStore()
        let old = try keys.add(credential: "01OLDAPPROVER0000000000000")
        let server = try TestServer(replies: [
            TestServer.reply(200, terminalList),
            TestServer.reply(400, #"{"error":"that enrollment code is not valid"}"#),
        ])
        let out = flows(server, keys: keys, ui: FakeUI(codes: ["ABCD-EFGH-JKMN-PQRS"])).enroll(name: "work")
        server.stop()
        XCTAssertEqual(out, .failed(Notice(title: "Couldn't enroll", body: "The daemon refused: that enrollment code is not valid.",
                                           succeeded: false)))
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
        let out = f.forget()
        XCTAssertEqual(out, .done(Notice(
            title: "Key deleted",
            body: "This Mac's Touch ID approver key is deleted, but the daemon still lists its credential. To remove it, run this as the daemon's account:",
            setApart: ["seamlessd approvers remove 01APPROVERID000000000000AA"], succeeded: true, instructs: true)))
        XCTAssertFalse(try XCTUnwrap(out.notice).autoCloses, "the alert carries an instruction and stays open")
        XCTAssertEqual(out.message, "This Mac's Touch ID approver key is deleted, but the daemon still lists its credential. To remove it, run this as the daemon's account:\n  seamlessd approvers remove 01APPROVERID000000000000AA")
        XCTAssertTrue(try keys.keys().isEmpty)
        XCTAssertEqual(ui.forgetAsked, [["01APPROVERID000000000000AA"], ["01APPROVERID000000000000AA"]])
        XCTAssertEqual(f.forget(), .done(Notice(title: "No key to delete",
                                                body: "No Touch ID approver key is stored on this Mac, so there is nothing to delete.",
                                                succeeded: true, changedNothing: true)))
    }

    /// A key that was never enrolled is deleted with nothing left to do.
    func testForgetAKeyNeverEnrolled() throws {
        let keys = SoftwareKeyStore()
        _ = try keys.create()
        let ui = FakeUI()
        let f = Flows(transport: UnixSocketTransport(path: "/nonexistent", userAgent: "t", admit: { _ in }), keys: keys, ui: ui)
        let out = f.forget()
        XCTAssertEqual(out, .done(Notice(title: "Key deleted", body: "This Mac's Touch ID approver key is deleted.", succeeded: true)))
        XCTAssertFalse(try XCTUnwrap(out.notice).autoCloses)
        XCTAssertEqual(ui.forgetAsked, [[]])
    }
}

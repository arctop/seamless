import Foundation
@testable import SeamApproveCore
import XCTest

/// The words of every alert and dialog the helper shows, built from facts.
final class PresentationTests: XCTestCase {
    static let newYork = TimeZone(identifier: "America/New_York")!

    /// 10:00 AM on Saturday 2026-11-07 in New York.
    static func clock(_ locale: String = "en_US", now: String = "2026-11-07T15:00:00Z") -> WallClock {
        WallClock(now: { ISO8601DateFormatter().date(from: now)! }, timeZone: newYork, locale: Locale(identifier: locale))
    }

    // MARK: times

    func testTimePhrases() {
        let c = Self.clock()
        let cases: [(String, String)] = [
            ("2026-11-07T19:09:00Z", "14:09"),
            ("2026-11-07T19:09:00.250Z", "14:09"),
            ("2026-11-08T14:15:00Z", "tomorrow 09:15"),
            ("2026-11-06T23:40:00Z", "yesterday 18:40"),
            ("2026-11-09T14:15:00Z", "Mon, Nov 9, 09:15"),
            ("2027-01-02T14:15:00Z", "Sat, Jan 2, 2027, 09:15"),
            ("not a\ntime", "not a time"),
        ]
        for (at, want) in cases {
            XCTAssertEqual(c.phrase(at), want, at)
        }
    }

    /// The day is the owner's: 00:30 local is tomorrow at 23:30 even when
    /// both instants share a UTC date. The time is on the 24-hour clock in
    /// every locale (the CLI's, the console's and the agent's clock); the
    /// locale words only the day and date.
    func testTimePhrasesFollowTheZoneAndLocale() {
        let late = Self.clock(now: "2026-11-08T04:30:00Z") // 11:30 PM on Nov 7 in New York
        XCTAssertEqual(late.phrase("2026-11-08T05:30:00Z"), "tomorrow 00:30")
        let gb = Self.clock("en_GB")
        XCTAssertEqual(gb.phrase("2026-11-07T19:09:00Z"), "14:09")
        XCTAssertEqual(gb.phrase("2026-11-08T14:15:00Z"), "tomorrow 09:15")
        XCTAssertEqual(gb.phrase("2026-11-09T14:15:00Z"), "Mon 9 Nov, 09:15")
        // A 12-hour locale, and one with other digits, still read 24-hour ASCII.
        XCTAssertEqual(Self.clock("en_US").phrase("2026-11-08T07:53:00Z"), "tomorrow 02:53")
        XCTAssertEqual(Self.clock("ar_EG").phrase("2026-11-07T21:49:00Z"), "16:49")
    }

    // MARK: alerts

    func testApprovedAlert() {
        let c = Self.clock()
        let n = Notices.approved(access: "read-write", project: "demo-vault", from: "", who: "Claude Code 2.1.319",
                                 expires: "2026-11-07T19:09:00Z", clock: c)
        XCTAssertEqual(n, Notice(title: "Approved", body: "Read-write access to demo-vault is open to Claude Code 2.1.319 until 14:09.",
                                 succeeded: true))
        XCTAssertFalse(n.autoCloses, "what was opened, and until when, stays until the owner dismisses it")
        XCTAssertEqual(Notices.approved(access: "read-only", project: "acme", from: "beta", who: "Codex", expires: "2026-11-08T14:15:00Z", clock: c).body,
                       "Read-only access to acme from project beta is open to Codex until tomorrow 09:15.")
        XCTAssertEqual(Notices.approved(access: "", project: "", from: "", who: "process 66124", expires: nil, clock: c).body,
                       "Access to the project is open to process 66124.")
    }

    func testEnrolledAlerts() {
        let first = Notices.enrolled(name: "Owner\u{2019}s MacBook", removals: [])
        XCTAssertEqual(first, Notice(title: "Touch ID enrolled",
                                     body: "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as \u{201C}Owner\u{2019}s MacBook\u{201D}.",
                                     succeeded: true))
        XCTAssertFalse(first.autoCloses, "a key enrolled stays until the owner dismisses it")

        let two = Notices.enrolled(name: "Touch ID", removals: ["sudo -k -u _seamless seamlessd approvers remove 01A", "sudo -k -u _seamless seamlessd approvers remove 01B"])
        XCTAssertEqual(two.body, "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as \u{201C}Touch ID\u{201D}. "
            + "The keys it replaces are deleted from this Mac, but the daemon still lists their credentials. To remove them, run these in your own terminal:")
        XCTAssertEqual(two.setApart.count, 2)
        XCTAssertFalse(two.autoCloses)

        let replaced = Notices.enrolled(name: "Touch ID", removals: [], retired: 1)
        XCTAssertEqual(replaced, Notice(title: "Touch ID enrolled",
                                        body: "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as \u{201C}Touch ID\u{201D}. "
                                            + "It replaces the key this Mac enrolled before, which is deleted from this Mac and from the daemon.",
                                        succeeded: true))
        XCTAssertFalse(replaced.autoCloses)
        XCTAssertTrue(Notices.enrolled(name: "Touch ID", removals: [], retired: 2).body
            .hasSuffix(" It replaces the keys this Mac enrolled before, which are deleted from this Mac and from the daemon."))
    }

    func testKeyDeletedAlerts() {
        XCTAssertEqual(Notices.keyDeleted(credentials: []),
                       Notice(title: "Key deleted", body: "This Mac's Touch ID approver key is deleted.", succeeded: true))
        let one = Notices.keyDeleted(credentials: ["01M6TCPBXG2R08217WHWH2DJSB"])
        XCTAssertEqual(one, Notice(title: "Key deleted",
                                   body: "This Mac's Touch ID approver key is deleted, but the daemon still lists its credential. To remove it, run this as the daemon's account:",
                                   setApart: ["seamlessd approvers remove 01M6TCPBXG2R08217WHWH2DJSB"], succeeded: true, instructs: true))
        XCTAssertFalse(one.body.contains("01M6"), "the id is only in the set-apart command, never in the wrapped body")
        let two = Notices.keyDeleted(credentials: ["01A", "01B"])
        XCTAssertEqual(two.body, "This Mac's Touch ID approver keys are deleted, but the daemon still lists their credentials. To remove them, run these as the daemon's account:")
        XCTAssertEqual(two.setApart, ["seamlessd approvers remove 01A", "seamlessd approvers remove 01B"])
        XCTAssertEqual(Notices.noKey(), Notice(title: "No key to delete",
                                               body: "No Touch ID approver key is stored on this Mac, so there is nothing to delete.",
                                               succeeded: true, changedNothing: true))
        XCTAssertFalse(Notices.keyDeleted(credentials: []).autoCloses, "a key deleted stays until the owner dismisses it")
        XCTAssertTrue(Notices.noKey().autoCloses, "nothing changed: the one alert that closes by itself")
    }

    func testFailureAlerts() {
        XCTAssertEqual(Notices.failed(.approve, "The daemon refused: this request was already decided"),
                       Notice(title: "Couldn't approve", body: "The daemon refused: this request was already decided.", succeeded: false))
        XCTAssertEqual(Notices.failed(.enroll, "x").title, "Couldn't enroll")
        XCTAssertEqual(Notices.failed(.forget, "x").title, "Couldn't delete the key")
        // The owner's run: the fake-socket refusal now opens with a capital.
        do {
            try PeerGuard(ownUID: 501).admit(501)
            XCTFail("admitted its own uid")
        } catch {
            XCTAssertEqual(Notices.failed(.approve, "\(error)").body,
                           "The process serving the socket runs as this account (uid 501), not as the daemon's, so Seamless Approve will not talk to it.")
        }
        // A message from below (the HTTP parser, a socket path check) is made
        // a sentence where it is shown.
        XCTAssertEqual(Notices.failed(.approve, "the daemon's reply ended inside its headers").body,
                       "The daemon's reply ended inside its headers.")
        XCTAssertEqual(Notices.failed(.enroll, "Is seamlessd running?").body, "Is seamlessd running?")

        let usage = Notices.badArguments("--for \u{201C}1 hour\u{201D} is not a duration (session, 1h or 4h)")
        XCTAssertEqual(usage.title, "Couldn't start")
        XCTAssertEqual(usage.body, "Seamless Approve was started with arguments it does not take: --for \u{201C}1 hour\u{201D} is not a duration (session, 1h or 4h). It takes these:")
        XCTAssertEqual(usage.setApart.first, "usage: SeamApprove approve --socket <path> --request <id> --for <duration>")
        XCTAssertFalse(usage.autoCloses)
    }

    /// Only a success that changed nothing and asks nothing of the owner
    /// closes by itself.
    func testAutoCloseRule() {
        XCTAssertEqual(Notice.autoCloseSeconds, 6)
        XCTAssertTrue(Notice(title: "t", body: "b.", succeeded: true, changedNothing: true).autoCloses)
        XCTAssertFalse(Notice(title: "t", body: "b.", succeeded: true).autoCloses)
        XCTAssertFalse(Notice(title: "t", body: "b.", succeeded: true, instructs: true, changedNothing: true).autoCloses)
        XCTAssertFalse(Notice(title: "t", body: "b.", succeeded: false, changedNothing: true).autoCloses)
        XCTAssertFalse(Notice(title: "t", body: "b.", succeeded: true, instructs: true).autoCloses)
        XCTAssertFalse(Notice(title: "t", body: "b.", succeeded: false).autoCloses)
        XCTAssertFalse(Notice(title: "t", body: "b.", succeeded: false, instructs: true).autoCloses)
    }

    /// Every alert body is a sentence: a capital letter first, a full stop,
    /// question mark or colon (before set-apart lines) last.
    func testEveryAlertIsASentence() {
        let c = Self.clock()
        let all: [Notice] = [
            Notices.approved(access: "read-write", project: "demo-vault", from: "", who: "Claude Code 2.1.319", expires: "2026-11-07T19:09:00Z", clock: c),
            Notices.enrolled(name: "Touch ID", removals: []),
            Notices.enrolled(name: "Touch ID", removals: ["x"]),
            Notices.enrolled(name: "Touch ID", removals: [], retired: 1),
            Notices.enrolled(name: "Touch ID", removals: ["x"], retired: 1),
            Notices.keyDeleted(credentials: []),
            Notices.keyDeleted(credentials: ["01A"]),
            Notices.noKey(),
            Notices.failed(.approve, "no daemon socket was given"),
            Notices.badArguments("no command given"),
        ]
        for n in all {
            XCTAssertTrue(n.body.first?.isUppercase == true, n.body)
            XCTAssertTrue(n.setApart.isEmpty ? ".?".contains(n.body.last!) : n.body.hasSuffix(":"), n.body)
            XCTAssertFalse(n.title.contains("Seamless Approve"), "the title says what happened: \(n.title)")
        }
    }

    func testAlertBodyKeepsAQuotedPathsFirstSlashOnItsLine() {
        let n = Notices.failed(.enroll, "There is no daemon socket at \u{201C}/tmp/nope\u{201D}.")
        XCTAssertEqual(n.alertBody, "There is no daemon socket at \u{201C}/\u{2060}tmp/nope\u{201D}.")
        XCTAssertEqual(n.text, "There is no daemon socket at \u{201C}/tmp/nope\u{201D}.", "stderr keeps the path as it is")

        let plain = Notices.failed(.approve, "the daemon refused: no such access request")
        XCTAssertEqual(plain.alertBody, plain.body, "a body with no quoted path is shown as it is")
    }

    // MARK: dialogs

    func testEnrollCodePrompt() {
        let command = "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code touchid"
        let p = Prompts.enrollCode(command: command, key: "Y2WK-2PG4-B7DB-FATZ", problem: nil)
        XCTAssertEqual(p, Prompt(title: "Enroll this Mac's Touch ID with Seamless",
                                 body: "Run this command in your own terminal, one your agents cannot read. It asks for your password, then prints a one-time enrollment code for this Mac's new key, Y2WK-2PG4-B7DB-FATZ. "
                                     + "Check that the key it prints is this one, then type the code below, with or without its dashes. The code works once, for 10 minutes, and only for this key.",
                                 setApart: [command], placeholder: "XXXX-XXXX-XXXX-XXXX", accept: "Enroll", cancel: "Cancel"))
        XCTAssertFalse(p.body.contains("sudo"), "the command is set apart, never wrapped into the body")
        XCTAssertEqual(Prompts.enrollCode(command: command, key: "Y2WK-2PG4-B7DB-FATZ", problem: CodeProblem.notACode).body,
                       "That is not an enrollment code. A code has 16 letters and digits, printed as XXXX-XXXX-XXXX-XXXX.\n\n" + p.body)
    }

    func testCopyableSetApartLines() {
        let command = "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code touchid"
        XCTAssertTrue(Prompts.enrollCode(command: command, key: "Y2WK-2PG4-B7DB-FATZ", problem: nil).copyable, "the enrollment command is copied by a click")
        XCTAssertFalse(Prompts.confirmForget(keptByDaemon: true).copyable, "nothing set apart, nothing to copy")
        XCTAssertTrue(Notices.enrolled(name: "Touch ID", removals: ["seamlessd approvers remove 01A"]).copyable)
        XCTAssertTrue(Notices.keyDeleted(credentials: ["01A"]).copyable)
        XCTAssertFalse(Notices.keyDeleted(credentials: []).copyable)
        XCTAssertFalse(Notices.badArguments("no command given").copyable, "a usage list is lines to match, not commands to run")

        XCTAssertEqual(CopyHint.before(lines: 1), "Click the command to copy it.")
        XCTAssertEqual(CopyHint.before(lines: 2), "Click the commands to copy them.")
        XCTAssertEqual(CopyHint.after, "Copied to the clipboard.")
        XCTAssertEqual(CopyHint.clipboard([command]), command, "no trailing newline: a paste does not run it")
        XCTAssertEqual(CopyHint.clipboard(["seamlessd approvers remove 01A", "seamlessd approvers remove 01B"]),
                       "seamlessd approvers remove 01A\nseamlessd approvers remove 01B")
    }

    func testConfirmForgetPrompt() {
        XCTAssertEqual(Prompts.confirmForget(keptByDaemon: false),
                       Prompt(title: "Delete this Mac's Touch ID approver key?",
                              body: "Seamless Approve will no longer be able to approve access from this Mac until you enroll again with seam touchid enroll.",
                              setApart: [], placeholder: nil, accept: "Delete Key", cancel: "Cancel"))
        XCTAssertEqual(Prompts.confirmForget(keptByDaemon: true).body,
                       "Seamless Approve will no longer be able to approve access from this Mac until you enroll again with seam touchid enroll. "
                           + "The daemon keeps the key's credential until its account removes it. The next window shows the command.")
    }

    /// The code as typed or pasted, in the form the daemon parses
    /// (store.NormalizeEnrollCode): any case, with or without dashes, spaces
    /// or tabs, and a line break a terminal paste brings at either end.
    func testTypedCode() throws {
        let cases: [(String, String)] = [
            ("ABCD-EFGH-JKMN-PQRS", "ABCDEFGHJKMNPQRS"),
            ("ABCDEFGHJKMNPQRS", "ABCDEFGHJKMNPQRS"),
            ("abcdefghjkmnpqrs", "ABCDEFGHJKMNPQRS"),
            ("abcd efgh\tjkmn-pqrs", "ABCDEFGHJKMNPQRS"),
            ("ABCD-EFGH-JKMN-PQRS\n", "ABCDEFGHJKMNPQRS"),
            ("\r\n abcd-efgh-jkmn-pqrs \r\n", "ABCDEFGHJKMNPQRS"),
        ]
        for (typed, want) in cases {
            let got = try Flows.typedCode(typed)
            XCTAssertEqual(got, want, typed)
            XCTAssertTrue(Messages.isEnrollCode(got), typed)
            XCTAssertEqual(try Messages.normalizeEnrollCode(got), got, "the daemon's normalization leaves it as it is")
        }
        // A line break inside the code is not the code's.
        XCTAssertFalse(Messages.isEnrollCode(try Flows.typedCode("ABCD-EFGH\nJKMN-PQRS")))
        XCTAssertThrowsError(try Flows.typedCode("ABCD\u{00A0}EFGH-JKMN-PQRS"))
    }

    func testSentence() {
        XCTAssertEqual(Text.sentence("the socket path is not absolute"), "The socket path is not absolute.")
        XCTAssertEqual(Text.sentence("Done."), "Done.")
        XCTAssertEqual(Text.sentence("is it running?"), "Is it running?")
        XCTAssertEqual(Text.sentence("cut\u{2026}"), "Cut\u{2026}")
        XCTAssertEqual(Text.sentence("--for is required"), "--for is required.")
        XCTAssertEqual(Text.sentence(""), "")
        XCTAssertEqual(Text.upperFirst("read-only"), "Read-only")
    }
}

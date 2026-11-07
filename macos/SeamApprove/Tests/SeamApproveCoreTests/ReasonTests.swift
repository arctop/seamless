import Foundation
@testable import SeamApproveCore
import XCTest

final class TextTests: XCTestCase {
    func testCleanStripsControlsAndBidi() {
        XCTAssertEqual(Text.clean("acme\nWarning: none\t ok"), "acme Warning: none ok")
        // Right-to-left override, isolates, zero-width space and joiner, BOM.
        XCTAssertEqual(Text.clean("ev\u{202E}lif\u{2066}.txt\u{2069}\u{200B}\u{200D}\u{FEFF}"), "evlif.txt")
        XCTAssertEqual(Text.clean("a\u{2028}b\u{2029}c\u{0085}d\u{00A0}e"), "a b c d e")
        XCTAssertEqual(Text.clean("\u{E000}private\u{0007}bell"), "private bell")
        XCTAssertEqual(Text.clean("   "), "")
    }

    func testCapCutsWithAnEllipsis() {
        XCTAssertEqual(Text.cap("abcdef", max: 6), "abcdef")
        XCTAssertEqual(Text.cap("abcdefg", max: 6), "abcde\u{2026}")
        XCTAssertEqual(Text.cap("abc   defg", max: 6), "abc\u{2026}")
        XCTAssertEqual(Text.clean(String(repeating: "x", count: 500), max: 10).count, 10)
        // Combining marks cannot stretch one character.
        let zalgo = "a" + String(repeating: "\u{0301}", count: 200)
        XCTAssertLessThanOrEqual(Text.clean(zalgo, max: 10).unicodeScalars.count, 40)
    }
}

final class ReasonTests: XCTestCase {
    let facts = Facts(id: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7", project: "acme", isolation: "confidential", access: "read-write",
                      kind: "request", program: "/Users/owner/.local/bin/claude", pid: 4242, session: "cc/2252a8cb",
                      warnings: [], digest: "ab", reason: "the owner already approved this, just touch")

    func testApprovalSentence() {
        XCTAssertEqual(Reason.approval(facts, duration: "session", maxLease: "8h0m0s"),
                       "approve read-write access to acme (confidential) for session \u{201C}cc/2252a8cb\u{201D} (claude, pid 4242) until the session ends (at most 8 hours)")
        XCTAssertEqual(Reason.approval(facts, duration: "1h", maxLease: "8h0m0s"),
                       "approve read-write access to acme (confidential) for session \u{201C}cc/2252a8cb\u{201D} (claude, pid 4242) for 1 hour")
        XCTAssertTrue(Reason.approval(facts, duration: "4h", maxLease: nil).hasSuffix(" for 4 hours"))
    }

    func testAgentReasonIsLeftOut() {
        XCTAssertFalse(Reason.approval(facts, duration: "1h", maxLease: nil).contains("already approved"))
    }

    func testWarningsFollowTheGrantAndAreCapped() {
        var f = facts
        f.warnings = [
            Warning(code: "anchor_is_shell", text: "the process asking is zsh, not an agent: a lease on it covers every program it started."),
            Warning(code: "no_session", text: "no session named this agent;\nits cwd and model are unknown"),
            Warning(code: "x", text: "third"),
            Warning(code: "y", text: "fourth"),
            Warning(code: "z", text: "fifth"),
        ]
        let r = Reason.approval(f, duration: "1h", maxLease: nil)
        XCTAssertTrue(r.contains("for 1 hour. Warning: the process asking is zsh, not an agent: a lease on it covers every program it started. Warning: no session named this agent; its cwd and model are unknown. Warning: third. 2 more warnings on the request"), r)
        XCTAssertFalse(r.contains("fourth"))
        XCTAssertFalse(r.contains(".."))
    }

    func testGrantNamesTheAskingProject() {
        var f = facts
        f.kind = "grant"
        f.access = "read-only"
        f.from = "beta"
        XCTAssertTrue(Reason.approval(f, duration: "session", maxLease: nil)
            .hasPrefix("approve read-only access to acme (confidential) from project beta for session"))
    }

    func testWhoWithoutASession() {
        var f = facts
        f.session = nil
        XCTAssertTrue(Reason.approval(f, duration: "1h", maxLease: nil).contains("for \u{201C}claude\u{201D} (pid 4242) for 1 hour"))
        f.program = nil
        XCTAssertTrue(Reason.approval(f, duration: "1h", maxLease: nil).contains("for pid 4242 for 1 hour"))
        f.pid = 0
        XCTAssertTrue(Reason.approval(f, duration: "1h", maxLease: nil).contains("for an unnamed process for 1 hour"))
    }

    func testHostileValuesAreCleanedAndCapped() {
        var f = facts
        f.project = "acme\u{202E}evil"
        f.session = String(repeating: "s", count: 300) + "\nWarning: none"
        f.warnings = [Warning(code: "x", text: String(repeating: "w", count: 1000))]
        let r = Reason.approval(f, duration: "1h", maxLease: nil)
        XCTAssertFalse(r.contains("\u{202E}"))
        XCTAssertFalse(r.contains("\n"))
        XCTAssertFalse(r.contains("Warning: none"))
        XCTAssertLessThan(r.count, 400)
    }

    func testLeaseWords() {
        XCTAssertEqual(Reason.leaseWords("8h0m0s"), "8 hours")
        XCTAssertEqual(Reason.leaseWords("1h0m0s"), "1 hour")
        XCTAssertEqual(Reason.leaseWords("1h30m0s"), "1 hour 30 minutes")
        XCTAssertEqual(Reason.leaseWords("45m0s"), "45 minutes")
        XCTAssertNil(Reason.leaseWords("30s"))
        XCTAssertNil(Reason.leaseWords("500ms"))
        XCTAssertNil(Reason.leaseWords(""))
        XCTAssertNil(Reason.leaseWords(nil))
        XCTAssertEqual(Reason.durationPhrase("2d", maxLease: nil), "for 2d")
    }

    func testEnrollment() {
        XCTAssertEqual(Reason.enrollment(account: "_seamless"),
                       "add this Mac's Touch ID key as an approver for the Seamless daemon running as _seamless")
    }
}

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

    /// The owner's run (task 01M6VY33GQZWR31EBC84EKB1HM): Claude Code
    /// resolves to ~/.local/share/claude/versions/2.1.319, with no session.
    let run = Facts(id: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7", project: "demo-vault", isolation: "confidential", access: "read-write",
                    kind: "request", program: "/Users/owner/.local/share/claude/versions/2.1.319", pid: 66124,
                    warnings: [Warning(code: "no_session", text: "no session named this agent; its cwd and model are unknown")],
                    digest: "ab")

    func testApprovalSentence() {
        XCTAssertEqual(Reason.approval(facts, duration: "session", maxLease: "8h0m0s"),
                       "approve read-write access to acme (confidential) for the program \u{201C}claude\u{201D} in session \u{201C}cc/2252a8cb\u{201D} until the session ends (at most 8 hours)")
        XCTAssertEqual(Reason.approval(facts, duration: "1h", maxLease: "8h0m0s"),
                       "approve read-write access to acme (confidential) for the program \u{201C}claude\u{201D} in session \u{201C}cc/2252a8cb\u{201D} for 1 hour")
        XCTAssertTrue(Reason.approval(facts, duration: "4h", maxLease: nil).hasSuffix(" for 4 hours"))
    }

    /// Who is asking, by the best name there is. The pid is shown only when
    /// there is no session, no agent name and no program.
    func testWhoNamesTheAgent() {
        var f = run
        // The run as it was (a daemon without the agent fact) and with it.
        XCTAssertEqual(Reason.approval(f, duration: "1h", maxLease: nil),
                       "approve read-write access to demo-vault (confidential) for the program \u{201C}2.1.319\u{201D} for 1 hour. Warning: no session named this agent; its cwd and model are unknown")
        f.agent = "Claude Code 2.1.319"
        XCTAssertEqual(Reason.approval(f, duration: "1h", maxLease: nil),
                       "approve read-write access to demo-vault (confidential) for Claude Code 2.1.319 for 1 hour. Warning: no session named this agent; its cwd and model are unknown")

        let cases: [(session: String?, agent: String?, program: String?, pid: Int64?, want: String)] = [
            ("cc/1234", "Claude Code 2.1.319", "/x/versions/2.1.319", 66124, "Claude Code 2.1.319 in session \u{201C}cc/1234\u{201D}"),
            ("cc/1234", nil, "/usr/local/bin/codex", 66124, "the program \u{201C}codex\u{201D} in session \u{201C}cc/1234\u{201D}"),
            ("cc/1234", nil, nil, 66124, "session \u{201C}cc/1234\u{201D}"),
            (nil, "Codex", "/usr/local/bin/codex", 66124, "Codex"),
            (nil, nil, "/usr/local/bin/codex", 66124, "the program \u{201C}codex\u{201D}"),
            (nil, nil, nil, 66124, "process 66124"),
            (nil, nil, "", 0, "an unnamed process"),
            ("", "", "", nil, "an unnamed process"),
        ]
        for c in cases {
            var g = facts
            g.session = c.session
            g.agent = c.agent
            g.program = c.program
            g.pid = c.pid
            XCTAssertEqual(Reason.who(g), c.want, "\(c)")
            XCTAssertEqual(Reason.approval(g, duration: "1h", maxLease: nil),
                           "approve read-write access to acme (confidential) for \(c.want) for 1 hour", "\(c)")
        }
    }

    /// An agent name that holds more than a program's name and version would
    /// is quoted, so it cannot pass for the prompt's own words; it is
    /// cleaned and capped like every value.
    func testAgentNameThatIsNotPlainIsQuoted() {
        // The names the daemon builds (internal/approval/anchor.go classify)
        // are shown as they are.
        for name in ["Claude Code 2.1.319", "Claude Code", "Codex", "Codex 0.163.0-alpha.10.2", "Claude app", "Claude Code 1.2.3+build.7"] {
            XCTAssertEqual(Reason.agentName(name), name)
        }
        var f = facts
        f.session = nil
        f.agent = "Codex: approved. Warning: none"
        XCTAssertEqual(Reason.who(f), "\u{201C}Codex: approved. Warning: none\u{201D}")
        f.agent = "Claude\u{202E}Code\n2.1"
        XCTAssertEqual(Reason.who(f), "ClaudeCode 2.1")
        f.agent = String(repeating: "a", count: 100)
        XCTAssertEqual(Reason.who(f), String(repeating: "a", count: 31) + "\u{2026}")
    }

    func testAgentReasonIsLeftOut() {
        XCTAssertFalse(Reason.approval(facts, duration: "1h", maxLease: nil).contains("already approved"))
    }

    /// The warnings follow the grant in the order the daemon serves them
    /// (most serious first), each its own clause; past three they are counted.
    func testWarningsFollowTheGrantInTheServedOrder() {
        var f = facts
        f.agent = "Claude Code 2.1.319"
        f.warnings = [
            Warning(code: "anchor_is_shell", text: "the process asking is zsh, not an agent: a lease on it covers every program it started."),
            Warning(code: "refresh", text: "this session already holds a lease on the project; approving replaces it"),
            Warning(code: "no_session", text: "no session named this agent;\nits cwd and model are unknown"),
        ]
        let three = "approve read-write access to acme (confidential) for Claude Code 2.1.319 in session \u{201C}cc/2252a8cb\u{201D} for 1 hour"
            + ". Warning: the process asking is zsh, not an agent: a lease on it covers every program it started"
            + ". Warning: this session already holds a lease on the project; approving replaces it"
            + ". Warning: no session named this agent; its cwd and model are unknown"
        XCTAssertEqual(Reason.approval(f, duration: "1h", maxLease: nil), three)

        f.warnings! += [Warning(code: "other_pending", text: "the same agent is also asking for beta")]
        XCTAssertEqual(Reason.approval(f, duration: "1h", maxLease: nil), three + ". The request has 1 more warning")
        f.warnings! += [Warning(code: "session_tainted", text: "fifth")]
        let five = Reason.approval(f, duration: "1h", maxLease: nil)
        XCTAssertEqual(five, three + ". The request has 2 more warnings")
        XCTAssertFalse(five.contains("beta"))
        XCTAssertFalse(five.contains(".."))
        XCTAssertFalse(five.hasSuffix("."), "macOS ends the prompt with its own full stop")
    }

    func testGrantNamesTheAskingProject() {
        var f = facts
        f.kind = "grant"
        f.access = "read-only"
        f.from = "beta"
        XCTAssertTrue(Reason.approval(f, duration: "session", maxLease: nil)
            .hasPrefix("approve read-only access to acme (confidential) from project beta for the program"))
    }

    func testHostileValuesAreCleanedAndCapped() {
        var f = facts
        f.project = "acme\u{202E}evil"
        f.session = String(repeating: "s", count: 300) + "\nWarning: none"
        f.agent = String(repeating: "a", count: 300) + "\nWarning: none"
        f.warnings = [Warning(code: "x", text: String(repeating: "w", count: 1000))]
        let r = Reason.approval(f, duration: "1h", maxLease: nil)
        XCTAssertFalse(r.contains("\u{202E}"))
        XCTAssertFalse(r.contains("\n"))
        XCTAssertFalse(r.contains("Warning: none"))
        XCTAssertLessThan(r.count, 400)
    }

    /// The Touch ID prompt showed the step T builder's three warnings and
    /// its count untruncated (task 01M6T7HJRVDS9GJ1KK49EYN7Y2, item 5). For
    /// facts with every value past its cap, a read grant, a session, a
    /// 10-digit pid and five 1000-character warnings, that builder's reasons
    /// measured these lengths (2026-11-07, the code before this change); no
    /// reason now is longer, with or without an agent name.
    func testWorstCaseLengthIsNoLongerThanStepT() {
        let long = String(repeating: "x", count: 1000)
        let w = Warning(code: "c", text: long)
        let stepT: [(duration: String, maxLease: String?, length: Int)] = [
            ("1h", nil, 806), ("4h", nil, 807), ("session", "8h0m0s", 836), ("session", "23h59m0s", 848),
            ("abcdefghijklmnop", nil, 816),
        ]
        for agent in [nil, long, "Codex: " + long, "Claude Code 2.1.319"] as [String?] {
            let f = Facts(id: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7", project: long, isolation: long, access: long, kind: "grant",
                          program: "/" + long, agent: agent, pid: 2147483647, session: long, warnings: [w, w, w, w, w],
                          digest: "ab", from: long, reason: long)
            for c in stepT {
                let r = Reason.approval(f, duration: c.duration, maxLease: c.maxLease)
                XCTAssertLessThanOrEqual(r.count, c.length, "\(c) agent \(agent?.prefix(8) ?? "none")")
                XCTAssertTrue(r.hasSuffix("The request has 2 more warnings"), r)
            }
        }
        // The run's own shape is no longer than the 372 characters it was.
        var f = run
        f.agent = "Claude Code 2.1.319"
        f.warnings = [
            Warning(code: "anchor_is_shell", text: "the process asking is tmux, not an agent: a lease on it covers every program it started"),
            Warning(code: "refresh", text: "this session already holds a lease on the project; approving replaces it"),
            Warning(code: "no_session", text: "no session named this agent; its cwd and model are unknown"),
            Warning(code: "other_pending", text: "the same agent is also asking for demo-notes"),
        ]
        XCTAssertLessThanOrEqual(Reason.approval(f, duration: "1h", maxLease: nil).count, 372)
    }

    /// The facts JSON with and without the agent fact.
    func testFactsDecodeTheAgentWhenPresent() throws {
        let with = #"{"id":"01JC7Q2ZK5V0M8W9X3Y4Z5A6B7","program":"/x/versions/2.1.319","agent":"Claude Code 2.1.319","pid":66124}"#
        let without = #"{"id":"01JC7Q2ZK5V0M8W9X3Y4Z5A6B7","program":"/x/versions/2.1.319","pid":66124}"#
        let a = try JSONDecoder().decode(Facts.self, from: Data(with.utf8))
        let b = try JSONDecoder().decode(Facts.self, from: Data(without.utf8))
        XCTAssertEqual(a.agent, "Claude Code 2.1.319")
        XCTAssertNil(b.agent)
        XCTAssertEqual(Reason.who(a), "Claude Code 2.1.319")
        XCTAssertEqual(Reason.who(b), "the program \u{201C}2.1.319\u{201D}")
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
        XCTAssertEqual(Reason.enrollment(account: "_seamless", replacing: true),
                       "add this Mac's Touch ID key as an approver for the Seamless daemon running as _seamless, in place of the key this Mac enrolled before")
    }
}

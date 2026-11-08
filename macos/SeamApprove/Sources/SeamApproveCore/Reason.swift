import Foundation

/// The Touch ID prompt's text (LAContext.localizedReason). macOS puts the
/// app's name in front of it ("Seamless Approve is trying to ...") and a full
/// stop after it, so the reason is a verb phrase with no final full stop. It
/// is built only from the daemon's facts: what the owner confirms with a touch
/// is what the daemon will grant. It is display only: the bytes the key signs
/// are TouchIDApproveMessage's (Messages), built from the plan, never from
/// this text.
///
/// The agent's own reason (Facts.reason) is left out on purpose. It is the
/// one value the asking agent writes freely, and the system's authentication
/// prompt is where text that imitates a fact or a warning ("Warning: none",
/// "the owner already approved") would do the most harm. The owner has seen
/// it, labeled as the agent's words, in the dialog that led here.
///
/// Every value is cleaned (Text.clean) and capped. The grant comes first,
/// then the warnings in the order the daemon serves them (most serious
/// first), each its own clause, before anything else could push them out.
public enum Reason {
    /// At most this many warnings are spelled out; the rest are counted.
    public static let maxWarnings = 3

    /// The longest a warning's text is shown, in characters. With every value
    /// at its cap, a reason is no longer than the step T builder's was for
    /// the same facts (ReasonTests.testWorstCaseLengthIsNoLongerThanStepT).
    static let maxWarningText = 140

    public static func approval(_ f: Facts, duration: String, maxLease: String?) -> String {
        let access = Text.clean(f.access ?? "", max: 16)
        let project = Text.clean(f.project ?? "", max: 64)
        let isolation = Text.clean(f.isolation ?? "", max: 16)
        var s = "approve " + (access.isEmpty ? "" : access + " ") + "access to " + (project.isEmpty ? "a project" : project)
        if !isolation.isEmpty {
            s += " (\(isolation))"
        }
        if f.kind == "grant", let from = f.from, !Text.clean(from).isEmpty {
            s += " from project " + Text.clean(from, max: 64)
        }
        s += " for " + who(f) + " " + durationPhrase(duration, maxLease: maxLease)
        let warnings = (f.warnings ?? []).map(warningText).filter { !$0.isEmpty }
        for w in warnings.prefix(maxWarnings) {
            s += ". Warning: " + w
        }
        if warnings.count > maxWarnings {
            let more = warnings.count - maxWarnings
            s += ". The request has \(more) more warning\(more == 1 ? "" : "s")"
        }
        return s
    }

    /// The enrollment's prompt: the key is about to sign its first message.
    /// `replacing` is whether this Mac's earlier key signs too, to be retired.
    public static func enrollment(account: String, replacing: Bool = false) -> String {
        var s = "add this Mac's Touch ID key as an approver for the Seamless daemon running as " + Text.clean(account, max: 32)
        if replacing {
            s += ", in place of the key this Mac enrolled before"
        }
        return s
    }

    /// `who` names the asking agent by the best name the daemon gives: the
    /// agent's name ("Claude Code 2.1.319"), else its program's base name,
    /// with the session it works in when there is one. The pid is a name only
    /// when there is nothing better: no session, no agent name, no program.
    /// A daemon too old to send the agent's name gets the program's name.
    public static func who(_ f: Facts) -> String {
        let session = Text.clean(f.session ?? "", max: 48)
        let name = agentName(f.agent) ?? programName(f.program)
        if !session.isEmpty {
            let s = "session " + Text.quoted(session, max: 48)
            return name.map { $0 + " in " + s } ?? s
        }
        if let name = name {
            return name
        }
        if let pid = f.pid, pid > 0 {
            return "process \(pid)"
        }
        return "an unnamed process"
    }

    /// `agentName` is the daemon's name for the agent, as it is when it holds
    /// only what a program's name and version hold (ASCII letters, digits,
    /// spaces and . - _ +, and the ellipsis of a cut), else between quotes,
    /// so that no name can pass for the prompt's own words ("Codex:
    /// approved"). Nil when there is none.
    static func agentName(_ raw: String?) -> String? {
        let name = Text.clean(raw ?? "", max: 32)
        guard !name.isEmpty else { return nil }
        let plain = name.unicodeScalars.allSatisfy { u in
            (u.isASCII && (Validate.isAlnum(UInt8(u.value)) || " .-_+".unicodeScalars.contains(u))) || u == "\u{2026}"
        }
        return plain ? name : Text.quoted(name, max: 32)
    }

    /// `programName` is the program's base name, quoted, or nil.
    static func programName(_ raw: String?) -> String? {
        let program = Text.clean(basename(raw ?? ""), max: 32)
        return program.isEmpty ? nil : "the program " + Text.quoted(program, max: 32)
    }

    /// `durationPhrase` says how long the grant lasts, for the durations the
    /// daemon offers (approval.Durations); the session's cap is maxLease, a Go
    /// duration ("8h0m0s").
    public static func durationPhrase(_ d: String, maxLease: String?) -> String {
        switch d {
        case "session":
            if let cap = leaseWords(maxLease) {
                return "until the session ends (at most \(cap))"
            }
            return "until the session ends"
        case "1h":
            return "for 1 hour"
        case "4h":
            return "for 4 hours"
        default:
            return "for " + Text.clean(d, max: 16)
        }
    }

    /// `leaseWords` reads a Go time.Duration string of whole hours and
    /// minutes ("8h0m0s", "1h30m0s", "45m0s") as words, or nil.
    static func leaseWords(_ s: String?) -> String? {
        guard var rest = s?.trimmingCharacters(in: .whitespaces), !rest.isEmpty else { return nil }
        func take(_ unit: Character) -> Int? {
            guard let i = rest.firstIndex(of: unit), let n = Int(rest[..<i]), n >= 0 else { return nil }
            rest = String(rest[rest.index(after: i)...])
            return n
        }
        let h = rest.contains("h") ? take("h") : 0
        let m = rest.contains("m") ? take("m") : 0
        guard let hours = h, let minutes = m, rest == "0s" || rest.isEmpty else { return nil }
        var words: [String] = []
        if hours > 0 {
            words.append(hours == 1 ? "1 hour" : "\(hours) hours")
        }
        if minutes > 0 {
            words.append(minutes == 1 ? "1 minute" : "\(minutes) minutes")
        }
        return words.isEmpty ? nil : words.joined(separator: " ")
    }

    static func warningText(_ w: Warning) -> String {
        var t = Text.clean(w.text ?? w.code ?? "", max: maxWarningText)
        while t.hasSuffix(".") {
            t.removeLast()
        }
        return t
    }

    static func basename(_ path: String) -> String {
        let trimmed = path.hasSuffix("/") ? String(path.dropLast()) : path
        return trimmed.split(separator: "/", omittingEmptySubsequences: false).last.map(String.init) ?? trimmed
    }
}

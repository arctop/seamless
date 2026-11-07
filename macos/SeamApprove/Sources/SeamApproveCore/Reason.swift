import Foundation

/// The Touch ID prompt's text (LAContext.localizedReason). macOS puts the
/// app's name in front of it ("Seamless Approve is trying to ..."), so the
/// reason is a verb phrase. It is built only from the daemon's facts: what
/// the owner confirms with a touch is what the daemon will grant.
///
/// The agent's own reason (Facts.reason) is left out on purpose. It is the
/// one value the asking agent writes freely, and the system's authentication
/// prompt is where text that imitates a fact or a warning ("Warning: none",
/// "the owner already approved") would do the most harm. The owner has seen
/// it, labeled as the agent's words, in the dialog that led here.
///
/// Every value is cleaned (Text.clean) and capped, and warnings come right
/// after the grant itself, before anything else could push them out.
public enum Reason {
    /// At most this many warnings are spelled out; the rest are counted.
    public static let maxWarnings = 3

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
            s += ". \(more) more warning\(more == 1 ? "" : "s") on the request"
        }
        return s
    }

    /// The enrolment's prompt: the key is about to sign its first message.
    public static func enrolment(account: String) -> String {
        "add this Mac's Touch ID key as an approver for the Seamless daemon running as " + Text.clean(account, max: 32)
    }

    /// `who` names the asking agent: its session, then the program and pid
    /// the kernel reported.
    static func who(_ f: Facts) -> String {
        let session = Text.clean(f.session ?? "", max: 48)
        let program = Text.clean(basename(f.program ?? ""), max: 32)
        let pid = (f.pid ?? 0) > 0 ? String(f.pid!) : nil
        var agent = ""
        switch (program.isEmpty, pid) {
        case (false, let p?):
            agent = "\(program), pid \(p)"
        case (false, nil):
            agent = program
        case (true, let p?):
            agent = "pid \(p)"
        case (true, nil):
            agent = ""
        }
        if !session.isEmpty {
            return "session " + Text.quoted(session, max: 48) + (agent.isEmpty ? "" : " (\(agent))")
        }
        if !program.isEmpty {
            return Text.quoted(program, max: 32) + (pid.map { " (pid \($0))" } ?? "")
        }
        if let p = pid {
            return "pid \(p)"
        }
        return "an unnamed process"
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
        var t = Text.clean(w.text ?? w.code ?? "", max: 140)
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

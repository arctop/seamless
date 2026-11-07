import Foundation

/// One run of the helper. LaunchServices starts it (`open -n -a <app> --args
/// ...`), so argv is all it is told -- and it trusts argv for nothing but
/// where to look: the request's facts and challenge come from the daemon, and
/// the enrollment code from the owner's own typing in the helper's dialog.
public enum Command: Equatable {
    /// Approve one access request for one duration.
    case approve(socket: String, request: String, duration: String)
    /// Create this Mac's key and enroll it with the daemon. `name` is the
    /// label the daemon shows; nil picks this Mac's name.
    case enroll(socket: String, name: String?)
    /// Delete this Mac's key from the keychain.
    case forget
}

/// A command line the helper refuses.
public struct UsageError: Error, Equatable, CustomStringConvertible {
    public let description: String

    public init(_ description: String) {
        self.description = description
    }
}

public enum Arguments {
    public static let usage = """
        usage: SeamApprove approve --socket <path> --request <id> --for <duration>
               SeamApprove enroll --socket <path> [--name <label>]
               SeamApprove forget
        The enrollment code is typed into the helper's own dialog, never passed here.
        """

    /// The longest approver name the daemon accepts, in Unicode code points
    /// (store.MaxApproverNameRunes; Go counts runes).
    public static let maxNameScalars = 64

    /// `parse` reads argv without the program name. Each flag is given once,
    /// as `--flag value` or `--flag=value`; anything else is refused.
    public static func parse(_ argv: [String]) throws -> Command {
        // LaunchServices on old systems appended a process serial number.
        let args = argv.filter { !$0.hasPrefix("-psn_") }
        guard let verb = args.first else {
            throw UsageError("no command given")
        }
        let rest = Array(args.dropFirst())
        switch verb {
        case "approve":
            let f = try flags(rest, allowed: ["socket", "request", "for"])
            let socket = try required(f, "socket")
            let request = try required(f, "request")
            let duration = try required(f, "for")
            if let problem = Validate.socketPathProblem(socket) {
                throw UsageError(problem)
            }
            guard Validate.isRequestID(request) else {
                throw UsageError("--request \(Text.quoted(request)) is not a request id (26 upper-case letters and digits)")
            }
            guard Validate.isDuration(duration) else {
                throw UsageError("--for \(Text.quoted(duration)) is not a duration (session, 1h or 4h)")
            }
            return .approve(socket: socket, request: request, duration: duration)
        case "enroll":
            let f = try flags(rest, allowed: ["socket", "name"])
            let socket = try required(f, "socket")
            if let problem = Validate.socketPathProblem(socket) {
                throw UsageError(problem)
            }
            var name: String?
            if let raw = f["name"] {
                name = try approverName(raw)
            }
            return .enroll(socket: socket, name: name)
        case "forget":
            _ = try flags(rest, allowed: [])
            return .forget
        default:
            throw UsageError("unknown command \(Text.quoted(verb))")
        }
    }

    /// `approverName` is a label as the daemon will store it: one cleaned
    /// line of 1 to 64 code points.
    public static func approverName(_ raw: String) throws -> String {
        let name = Text.clean(raw, max: 1000)
        guard !name.isEmpty else {
            throw UsageError("--name is empty")
        }
        guard name.unicodeScalars.count <= maxNameScalars else {
            throw UsageError("--name is at most \(maxNameScalars) characters")
        }
        return name
    }

    /// `defaultApproverName` is the label used without --name: this Mac's
    /// name, cleaned and cut to whole characters within 64 code points, or
    /// "Touch ID" when nothing usable is left.
    public static func defaultApproverName(_ raw: String) -> String {
        var name = Text.clean(raw, max: maxNameScalars)
        while name.unicodeScalars.count > maxNameScalars {
            name.removeLast()
        }
        name = name.trimmingCharacters(in: .whitespaces)
        return name.isEmpty ? "Touch ID" : name
    }

    static func flags(_ rest: [String], allowed: Set<String>) throws -> [String: String] {
        var out: [String: String] = [:]
        var i = 0
        while i < rest.count {
            let arg = rest[i]
            guard arg.hasPrefix("--"), arg.count > 2 else {
                throw UsageError("unexpected argument \(Text.quoted(arg))")
            }
            var name = String(arg.dropFirst(2))
            var value: String?
            if let eq = name.firstIndex(of: "=") {
                value = String(name[name.index(after: eq)...])
                name = String(name[..<eq])
            }
            guard allowed.contains(name) else {
                throw UsageError("unknown flag \(Text.quoted("--" + name))")
            }
            guard out[name] == nil else {
                throw UsageError("--\(name) is given twice")
            }
            if value == nil {
                i += 1
                guard i < rest.count else {
                    throw UsageError("--\(name) needs a value")
                }
                value = rest[i]
            }
            out[name] = value
            i += 1
        }
        return out
    }

    static func required(_ f: [String: String], _ name: String) throws -> String {
        guard let v = f[name] else {
            throw UsageError("--\(name) is required")
        }
        return v
    }
}

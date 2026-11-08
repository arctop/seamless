import Foundation

// The daemon's JSON, as the console routes answer over the peer_auth socket
// (internal/console/access.go and approvers.go; the CLI's mirror is
// cmd/seam/access.go). Every field is optional here: a missing one is judged
// where it matters, not by the decoder.

/// One thing the owner should weigh before approving (approval.Warning).
public struct Warning: Decodable, Equatable {
    public var code: String?
    public var text: String?

    public init(code: String? = nil, text: String? = nil) {
        self.code = code
        self.text = text
    }
}

/// The facts of one access request (approval.Facts), as the daemon states
/// them. The helper shows these and nothing it was told on its command line.
public struct Facts: Decodable, Equatable {
    public var id: String?
    public var shortID: String?
    public var project: String?
    public var isolation: String?
    public var access: String?
    public var kind: String?
    public var program: String?
    /// `agent` is the daemon's readable name for the program ("Claude Code
    /// 2.1.319", "Codex"). A daemon that cannot name it, or one too old to
    /// know the field, sends none.
    public var agent: String?
    public var pid: Int64?
    public var uid: Int64?
    public var session: String?
    public var sessionID: String?
    public var cwd: String?
    public var repo: String?
    public var model: String?
    public var reason: String?
    public var warnings: [Warning]?
    public var requestedAt: String?
    public var expiresAt: String?
    public var digest: String?
    public var from: String?

    enum CodingKeys: String, CodingKey {
        case id, project, isolation, access, kind, program, agent, pid, uid, session, cwd, repo, model, reason, warnings, digest, from
        case shortID = "short_id"
        case sessionID = "session_id"
        case requestedAt = "requested_at"
        case expiresAt = "expires_at"
    }

    public init(id: String? = nil, project: String? = nil, isolation: String? = nil, access: String? = nil,
                kind: String? = nil, program: String? = nil, agent: String? = nil, pid: Int64? = nil, session: String? = nil,
                warnings: [Warning]? = nil, digest: String? = nil, from: String? = nil, reason: String? = nil) {
        self.id = id
        self.project = project
        self.isolation = isolation
        self.access = access
        self.kind = kind
        self.program = program
        self.agent = agent
        self.pid = pid
        self.session = session
        self.warnings = warnings
        self.digest = digest
        self.from = from
        self.reason = reason
    }
}

/// How the terminal path reaches its commit (console.TerminalApprover). The
/// helper needs only the account: the socket's peer must be it.
public struct Terminal: Decodable, Equatable {
    public var account: String?
    public var seamlessd: String?
    public var config: String?
}

/// GET /console/access/{id}?format=json.
public struct AccessOne: Decodable {
    public var request: Facts?
    public var status: String?
    public var page: String?
    public var terminal: Terminal?
    public var durations: [String]?
    public var maxLease: String?
    /// The request's challenge in standard base64: in the JSON of a pending
    /// request read over the peer_auth socket only.
    public var challenge: String?

    enum CodingKeys: String, CodingKey {
        case request, status, page, terminal, durations, challenge
        case maxLease = "max_lease"
    }
}

/// GET /console/access?format=json: the enrollment reads it for the daemon's
/// account before the owner types a code.
public struct AccessList: Decodable {
    public var terminal: Terminal?
}

/// POST /console/access/{id}/approve's answer. `from` is a read grant's
/// asking project; `expiresAt` is RFC 3339 in UTC.
public struct Approved: Decodable {
    public var approved: String?
    public var project: String?
    public var from: String?
    public var expiresAt: String?

    enum CodingKeys: String, CodingKey {
        case approved, project, from
        case expiresAt = "expires_at"
    }
}

/// POST /console/approvers/touchid/enroll's answer. `retired` lists the
/// earlier keys of this Mac the daemon retired with the enrollment; a daemon
/// from before that field, or one that retired none, sends no list.
public struct Enrolled: Decodable {
    public var id: String?
    public var name: String?
    public var retired: [String]?
}

/// Any route's refusal: {"error": "..."}.
public struct DaemonRefusal: Decodable {
    public var error: String?
}

/// What an approval signs and sends, judged from the daemon's answer.
public struct ApprovalPlan: Equatable {
    public let id: String
    public let challenge: Data
    public let digest: String
    public let access: String
    public let duration: String
    public let account: String
    public let facts: Facts
    public let maxLease: String?
}

public enum Plan {
    /// `approval` checks the daemon's answer for request `id` and duration
    /// `duration` before anything is signed: the request is the one asked
    /// for and still pending, the duration is one the daemon offers, the
    /// challenge, digest and access are present, and the daemon names its
    /// account (which the caller then checks against the socket's peer).
    public static func approval(_ one: AccessOne, id: String, duration: String) throws -> ApprovalPlan {
        guard let facts = one.request else {
            throw ApproverError.daemon("The daemon's answer carries no request.")
        }
        guard facts.id == id else {
            throw ApproverError.daemon("The daemon answered for another request (\(Text.quoted(facts.id ?? ""))).")
        }
        let short = Text.clean(facts.shortID ?? id, max: 26)
        guard let status = one.status, status == "pending" else {
            throw ApproverError.refused("Request \(short) is \(Text.clean(one.status ?? "in an unknown state", max: 32)), so there is nothing to approve.")
        }
        guard let offered = one.durations, offered.contains(duration) else {
            throw ApproverError.refused("The daemon does not offer the duration \(Text.quoted(duration)) for request \(short).")
        }
        guard let b64 = one.challenge, !b64.isEmpty else {
            throw ApproverError.daemon("The daemon sent no challenge for request \(short). Is this daemon too old for Touch ID approval?")
        }
        guard let challenge = Data(base64Encoded: b64), !challenge.isEmpty else {
            throw ApproverError.daemon("The daemon's challenge for request \(short) is not standard base64.")
        }
        guard let digest = facts.digest, Validate.isHexDigest(digest) else {
            throw ApproverError.daemon("The daemon sent no usable digest of request \(short)'s facts.")
        }
        guard let access = facts.access, !access.isEmpty else {
            throw ApproverError.daemon("The daemon did not say what access request \(short) asks for.")
        }
        let account = try daemonAccount(one.terminal)
        return ApprovalPlan(id: id, challenge: challenge, digest: digest, access: access, duration: duration,
                            account: account, facts: facts, maxLease: one.maxLease)
    }

    /// `daemonAccount` is the account the daemon says it runs as, checked
    /// for shape; the caller compares it with the socket's peer.
    public static func daemonAccount(_ t: Terminal?) throws -> String {
        guard let account = t?.account, !account.isEmpty else {
            throw ApproverError.daemon("The daemon did not name its own account, so Seamless Approve cannot check who is serving the socket.")
        }
        guard Validate.isAccountName(account) else {
            throw ApproverError.daemon("The daemon named \(Text.quoted(account)) as its account, which is not an account name.")
        }
        return account
    }
}

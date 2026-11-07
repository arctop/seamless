import Foundation

/// Why a run did not finish. Every case's message is fit to show the owner.
public enum ApproverError: Error, Equatable, CustomStringConvertible {
    /// A check refused (a decided request, a duration not offered, ...).
    case refused(String)
    /// The daemon's answer is unusable, or it refused with this message.
    case daemon(String)
    /// The socket's peer is not the daemon's account.
    case peer(String)
    /// The socket could not be reached or read.
    case transport(String)
    /// The key store is unavailable or failed.
    case keyStore(String)
    /// The owner cancelled (a dialog or the Touch ID prompt).
    case cancelled(String)

    public var description: String {
        switch self {
        case .refused(let m), .daemon(let m), .peer(let m), .transport(let m), .keyStore(let m), .cancelled(let m):
            return m
        }
    }
}

/// How a run ended: the process exits 0, 1 or 2 (`exitCode`).
public enum Outcome: Equatable {
    case done(String)
    case failed(String)
    case cancelled(String)

    public var exitCode: Int32 {
        switch self {
        case .done: return 0
        case .failed: return 1
        case .cancelled: return 2
        }
    }

    public var message: String {
        switch self {
        case .done(let m), .failed(let m), .cancelled(let m):
            return m
        }
    }
}

/// One key in the store: `handle` names it to the store (the keychain
/// item's application label, a hash of its public key), and `credential` is
/// the approver id the daemon returned at enrolment, or nil while the key
/// is not enrolled.
public struct StoredKey: Equatable {
    public let handle: Data
    public let credential: String?

    public init(handle: Data, credential: String?) {
        self.handle = handle
        self.credential = credential
    }
}

/// Where the approver key lives. Production keeps it in the Secure Enclave,
/// in the data protection keychain under an access group only the signed
/// helper may claim (SecureEnclaveKeyStore); tests use a software key.
public protocol KeyStore {
    /// `checkAvailable` throws ApproverError.keyStore when this copy of the
    /// helper has no key store (unsigned, or no Secure Enclave).
    func checkAvailable() throws
    func keys() throws -> [StoredKey]
    /// `create` makes a new key, not yet enrolled, and returns it with its
    /// raw public key (65 bytes: 0x04, X, Y).
    func create() throws -> (StoredKey, Data)
    /// `sign` asks for the owner's presence with `reason` and returns the
    /// ASN.1 DER ECDSA signature over SHA-256 of message. A cancelled prompt
    /// throws ApproverError.cancelled.
    func sign(_ message: Data, with key: StoredKey, reason: String) throws -> Data
    func setCredential(_ id: String, for key: StoredKey) throws
    func delete(_ key: StoredKey) throws
}

/// What the flows ask the owner, in the helper's own dialogs.
public protocol ApproverUI {
    /// `enrolCode` asks for the one-time code; nil is a cancel. `problem`
    /// says what was wrong with the last one typed.
    func enrolCode(command: String, problem: String?) -> String?
    /// `confirmForget` asks before the key is deleted.
    func confirmForget(credentials: [String]) -> Bool
}

/// The three runs, over a transport, a key store and a UI.
public struct Flows {
    public let transport: Transport
    public let keys: KeyStore
    public let ui: ApproverUI
    public let lookupAccount: (String) -> uid_t?
    public let defaultName: () -> String

    public init(transport: Transport, keys: KeyStore, ui: ApproverUI,
                lookupAccount: @escaping (String) -> uid_t? = uidOfAccount,
                defaultName: @escaping () -> String = { "Touch ID" }) {
        self.transport = transport
        self.keys = keys
        self.ui = ui
        self.lookupAccount = lookupAccount
        self.defaultName = defaultName
    }

    public static func run(_ body: () throws -> Outcome) -> Outcome {
        do {
            return try body()
        } catch ApproverError.cancelled(let m) {
            return .cancelled(m)
        } catch let e as ApproverError {
            return .failed(e.description)
        } catch {
            return .failed(Text.clean("\(error)"))
        }
    }

    // MARK: approve

    /// `approve` reads request `id` from the daemon, shows its facts in the
    /// Touch ID prompt, signs TouchIDApproveMessage and sends the approval.
    public func approve(id: String, duration: String) -> Outcome {
        Flows.run {
            try keys.checkAvailable()
            let key = try enrolledKey()
            let got = try transport.exchange(HTTPRequest(method: "GET", path: "/console/access/\(id)?format=json"))
            let one: AccessOne = try decode(got.response, what: "the request")
            let plan = try Plan.approval(one, id: id, duration: duration)
            try requireAccount(plan.account, isPeer: got.peerUID, lookup: lookupAccount)

            let reason = Reason.approval(plan.facts, duration: duration, maxLease: plan.maxLease)
            let message = Messages.touchIDApproveMessage(id: plan.id, challenge: plan.challenge, digest: plan.digest,
                                                         access: plan.access, duration: plan.duration)
            let signature: Data
            do {
                signature = try keys.sign(message, with: key, reason: reason)
            } catch ApproverError.cancelled {
                throw ApproverError.cancelled("Not approved: the Touch ID prompt was cancelled.")
            }

            let sent = try transport.exchange(HTTPRequest(method: "POST", path: "/console/access/\(id)/approve?format=json", form: [
                FormField("proof", "touchid"),
                FormField("credential", key.credential ?? ""),
                FormField("signature", signature.base64EncodedString()),
                FormField("duration", plan.duration),
                FormField("digest", plan.digest),
            ]))
            let ok: Approved = try decode(sent.response, what: "the approval")
            let project = Text.clean(ok.project ?? plan.facts.project ?? "the project", max: 64)
            let until = ok.expiresAt.map { " until " + Flows.localTime($0) } ?? ""
            return .done("Approved: \(project) is open to \(Reason.who(plan.facts))\(until).")
        }
    }

    // MARK: enroll

    /// `enroll` checks the socket is the daemon's, asks the owner for the
    /// enrolment code in the helper's own dialog, creates a key, signs
    /// TouchIDEnrolMessage with it and registers it. A key that does not end
    /// up enrolled is deleted; an older key is replaced once the new one is.
    public func enroll(name: String?) -> Outcome {
        Flows.run {
            try keys.checkAvailable()
            // Who serves the socket is settled before the owner types the
            // code: the code is the secret, and only the daemon may see it.
            let got = try transport.exchange(HTTPRequest(method: "GET", path: "/console/access?format=json"))
            let list: AccessList = try decode(got.response, what: "the daemon's account")
            let account = try Plan.daemonAccount(list.terminal)
            try requireAccount(account, isPeer: got.peerUID, lookup: lookupAccount)

            let code = try askCode(enrolCommand(list.terminal, account: account))
            let label = name ?? Arguments.defaultApproverName(defaultName())

            let older = try keys.keys()
            let (key, publicKey) = try keys.create()
            var enrolled = false
            defer {
                if !enrolled {
                    try? keys.delete(key)
                }
            }
            let message = try Messages.touchIDEnrolMessage(code: code, publicKey: publicKey)
            let signature: Data
            do {
                signature = try keys.sign(message, with: key, reason: Reason.enrolment(account: account))
            } catch ApproverError.cancelled {
                throw ApproverError.cancelled("Not enrolled: the Touch ID prompt was cancelled.")
            }
            let sent = try transport.exchange(HTTPRequest(method: "POST", path: "/console/approvers/touchid/enrol?format=json", form: [
                FormField("code", code),
                FormField("name", label),
                FormField("public_key", publicKey.base64EncodedString()),
                FormField("signature", signature.base64EncodedString()),
            ]))
            let ok: Enrolled = try decode(sent.response, what: "the enrolment")
            guard let id = ok.id, Validate.isCredentialID(id) else {
                throw ApproverError.daemon("the daemon enrolled the key but returned no usable approver id")
            }
            try keys.setCredential(id, for: key)
            enrolled = true
            var replaced = 0
            for old in older where old.handle != key.handle {
                if (try? keys.delete(old)) != nil {
                    replaced += 1
                }
            }
            let shown = Text.clean(ok.name ?? label, max: 64)
            var msg = "Enrolled this Mac's Touch ID as the approver \(Text.quoted(shown, max: 64)) (\(id))."
            if replaced > 0 {
                msg += " The key it replaces was deleted here; remove its old credential with seamlessd approvers remove, run as the daemon's account."
            }
            return .done(msg)
        }
    }

    func askCode(_ command: String) throws -> String {
        var problem: String?
        for _ in 0..<5 {
            guard let typed = ui.enrolCode(command: command, problem: problem) else {
                throw ApproverError.cancelled("Not enrolled: cancelled.")
            }
            let normalized: String
            do {
                normalized = try Messages.normalizeEnrolCode(typed)
            } catch {
                problem = "An enrolment code holds only letters, digits and dashes."
                continue
            }
            if Messages.isEnrolCode(normalized) {
                return normalized
            }
            problem = "That is not an enrolment code: it has 16 letters and digits, as XXXX-XXXX-XXXX-XXXX."
        }
        throw ApproverError.refused("Not enrolled: no valid enrolment code was typed.")
    }

    /// The command that mints a Touch ID code, as `seam approve` would run
    /// the daemon's seamlessd (cmd/seam/approve.go sudoArgv).
    func enrolCommand(_ t: Terminal?, account: String) -> String {
        var parts = ["sudo -k -u", account]
        if let config = t?.config, config.hasPrefix("/") {
            parts.append("env SEAMLESS_CONFIG=" + Text.clean(config, max: 120))
        }
        if let exe = t?.seamlessd, exe.hasPrefix("/") {
            parts.append(Text.clean(exe, max: 120))
        } else {
            parts.append("seamlessd")
        }
        parts.append("approvers enroll-code touchid")
        return parts.joined(separator: " ")
    }

    // MARK: forget

    /// `forget` deletes this Mac's key after the owner confirms. The daemon
    /// keeps the credential until the daemon's account removes it.
    public func forget() -> Outcome {
        Flows.run {
            try keys.checkAvailable()
            let stored = try keys.keys()
            if stored.isEmpty {
                return .done("No Touch ID approver key is stored on this Mac: nothing to forget.")
            }
            let ids = stored.compactMap { $0.credential }
            guard ui.confirmForget(credentials: ids) else {
                throw ApproverError.cancelled("Nothing was deleted.")
            }
            for k in stored {
                try keys.delete(k)
            }
            if ids.isEmpty {
                return .done("Deleted this Mac's Touch ID approver key.")
            }
            return .done("Deleted this Mac's Touch ID approver key. The daemon still lists it as \(ids.joined(separator: ", ")): remove it with seamlessd approvers remove, run as the daemon's account.")
        }
    }

    // MARK: helpers

    func enrolledKey() throws -> StoredKey {
        let enrolled = try keys.keys().filter { $0.credential != nil }
        switch enrolled.count {
        case 0:
            throw ApproverError.keyStore("No Touch ID approver key is enrolled on this Mac: run seam touchid enroll.")
        case 1:
            return enrolled[0]
        default:
            // seam has no forget command; a new enrolment deletes every older
            // key once the new one is enrolled (enroll, below).
            throw ApproverError.keyStore("Several Touch ID approver keys are stored on this Mac: run seam touchid enroll again, which keeps only the new key.")
        }
    }

    func decode<T: Decodable>(_ r: HTTPResponse, what: String) throws -> T {
        guard r.status == 200 else {
            if let refusal = try? JSONDecoder().decode(DaemonRefusal.self, from: r.body), let e = refusal.error, !e.isEmpty {
                throw ApproverError.daemon("The daemon refused: " + Text.clean(e, max: 300))
            }
            throw ApproverError.daemon("The daemon answered \(r.status) for \(what).")
        }
        do {
            return try JSONDecoder().decode(T.self, from: r.body)
        } catch {
            throw ApproverError.daemon("The daemon's answer for \(what) is not the JSON this helper reads.")
        }
    }

    /// `localTime` shows an RFC 3339 time in this Mac's time zone, or the
    /// cleaned text when it does not parse.
    static func localTime(_ rfc3339: String) -> String {
        let iso = ISO8601DateFormatter()
        guard let d = iso.date(from: rfc3339) else {
            return Text.clean(rfc3339, max: 40)
        }
        let f = DateFormatter()
        f.dateStyle = .none
        f.timeStyle = .short
        if !Calendar.current.isDateInToday(d) {
            f.dateStyle = .medium
        }
        return f.string(from: d)
    }
}

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

/// How a run ended: the process exits 0, 1 or 2 (`exitCode`). A finished or
/// failed run carries the alert that reports it; a cancel shows none.
public enum Outcome: Equatable {
    case done(Notice)
    case failed(Notice)
    case cancelled(String)

    public var exitCode: Int32 {
        switch self {
        case .done: return 0
        case .failed: return 1
        case .cancelled: return 2
        }
    }

    /// `notice` is the alert to show, nil for a cancel.
    public var notice: Notice? {
        switch self {
        case .done(let n), .failed(let n):
            return n
        case .cancelled:
            return nil
        }
    }

    /// `message` is the outcome as plain text, for stderr.
    public var message: String {
        switch self {
        case .done(let n), .failed(let n):
            return n.text
        case .cancelled(let m):
            return m
        }
    }

    /// `stderrLine` is the last line the app writes to stderr, without its
    /// newline. The `seam mcp-proxy` bridge reads it through `open --stderr`
    /// (the exit status does not survive LaunchServices) and reports a line
    /// that begins "SeamApprove: Not approved:" as a cancelled approval
    /// (cmd/seam/mcpproxy_approver.go approverOutcome). Both sides pin the
    /// cancelled line in a test: change them together.
    public var stderrLine: String { "SeamApprove: " + message }
}

/// One key in the store: `handle` names it to the store (the keychain
/// item's application label, a hash of its public key), and `credential` is
/// the approver id the daemon returned at enrollment, or nil while the key
/// is not enrolled.
public struct StoredKey: Equatable {
    public let handle: Data
    public let credential: String?

    public init(handle: Data, credential: String?) {
        self.handle = handle
        self.credential = credential
    }
}

/// One signature a run asks for: `message`, by `key`.
public struct Signing: Equatable {
    public let message: Data
    public let key: StoredKey

    public init(message: Data, key: StoredKey) {
        self.message = message
        self.key = key
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
    /// `signAll` signs each message with its key in one run of prompts: the
    /// owner's presence is asked for with `reason`, once where one check can
    /// cover every key. The first signature must succeed (a cancelled prompt
    /// throws ApproverError.cancelled); a later one that does not is nil.
    func signAll(_ signings: [Signing], reason: String) throws -> [Data?]
    func setCredential(_ id: String, for key: StoredKey) throws
    func delete(_ key: StoredKey) throws
}

/// What the flows ask the owner, in the helper's own dialogs.
public protocol ApproverUI {
    /// `enrollCode` asks for the one-time code; nil is a cancel. `problem`
    /// says what was wrong with the last one typed.
    func enrollCode(command: String, problem: String?) -> String?
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
    /// `clock` says the lease's end in the approval's alert.
    public let clock: WallClock

    public init(transport: Transport, keys: KeyStore, ui: ApproverUI,
                lookupAccount: @escaping (String) -> uid_t? = uidOfAccount,
                defaultName: @escaping () -> String = { "Touch ID" },
                clock: WallClock = .system) {
        self.transport = transport
        self.keys = keys
        self.ui = ui
        self.lookupAccount = lookupAccount
        self.defaultName = defaultName
        self.clock = clock
    }

    public static func run(_ flow: Flow, _ body: () throws -> Notice) -> Outcome {
        do {
            return .done(try body())
        } catch ApproverError.cancelled(let m) {
            return .cancelled(m)
        } catch let e as ApproverError {
            return .failed(Notices.failed(flow, e.description))
        } catch {
            return .failed(Notices.failed(flow, Text.clean("\(error)")))
        }
    }

    // MARK: approve

    /// `approve` reads request `id` from the daemon, shows its facts in the
    /// Touch ID prompt, signs TouchIDApproveMessage and sends the approval.
    public func approve(id: String, duration: String) -> Outcome {
        Flows.run(.approve) {
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
            let from = plan.facts.kind == "grant" ? (ok.from ?? plan.facts.from ?? "") : ""
            return Notices.approved(access: plan.access, project: ok.project ?? plan.facts.project ?? "", from: from,
                                    who: Reason.who(plan.facts), expires: ok.expiresAt, clock: clock)
        }
    }

    // MARK: enroll

    /// `enroll` checks the socket is the daemon's, asks the owner for the
    /// enrollment code in the helper's own dialog, creates a key, signs
    /// TouchIDEnrollMessage with it and registers it. A key that does not end
    /// up enrolled is deleted; an older key is replaced once the new one is.
    /// An older enrolled key also signs TouchIDReplaceMessage, so the daemon
    /// retires its credential with the enrollment, in the same transaction:
    /// only this Mac holds that key, so only this Mac's earlier key is ever
    /// retired. One the daemon does not retire is left for the owner to
    /// remove, with the command in the closing alert.
    public func enroll(name: String?) -> Outcome {
        Flows.run(.enroll) {
            try keys.checkAvailable()
            // Who serves the socket is settled before the owner types the
            // code: the code is the secret, and only the daemon may see it.
            let got = try transport.exchange(HTTPRequest(method: "GET", path: "/console/access?format=json"))
            let list: AccessList = try decode(got.response, what: "the daemon's account")
            let account = try Plan.daemonAccount(list.terminal)
            try requireAccount(account, isPeer: got.peerUID, lookup: lookupAccount)

            let code = try askCode(daemonCommand(list.terminal, account: account, verb: "enroll-code touchid"))
            let label = name ?? Arguments.defaultApproverName(defaultName())

            let older = try keys.keys()
            let (key, publicKey) = try keys.create()
            var enrolled = false
            defer {
                if !enrolled {
                    try? keys.delete(key)
                }
            }
            let replacing = older.filter { $0.handle != key.handle && $0.credential != nil }
            var signings = [Signing(message: try Messages.touchIDEnrollMessage(code: code, publicKey: publicKey), key: key)]
            for old in replacing {
                let retiring = try Messages.touchIDReplaceMessage(code: code, publicKey: publicKey, retiring: old.credential ?? "")
                signings.append(Signing(message: retiring, key: old))
            }
            let signatures: [Data?]
            do {
                signatures = try keys.signAll(signings, reason: Reason.enrollment(account: account, replacing: !replacing.isEmpty))
            } catch ApproverError.cancelled {
                throw ApproverError.cancelled("Not enrolled: the Touch ID prompt was cancelled.")
            }
            guard let first = signatures.first, let signature = first else {
                throw ApproverError.keyStore("Touch ID did not sign the enrollment.")
            }
            var form = [
                FormField("code", code),
                FormField("name", label),
                FormField("public_key", publicKey.base64EncodedString()),
                FormField("signature", signature.base64EncodedString()),
            ]
            // An earlier key that could not sign (its prompt was cancelled)
            // is not named: the daemon keeps it, and the alert says how to
            // remove it.
            for (old, sig) in zip(replacing, signatures.dropFirst()) {
                if let sig = sig, let gone = old.credential {
                    form.append(FormField("replace", gone))
                    form.append(FormField("replace_signature", sig.base64EncodedString()))
                }
            }
            let sent = try transport.exchange(HTTPRequest(method: "POST", path: "/console/approvers/touchid/enroll?format=json", form: form))
            let ok: Enrolled = try decode(sent.response, what: "the enrollment")
            guard let id = ok.id, Validate.isCredentialID(id) else {
                throw ApproverError.daemon("The daemon enrolled the key but returned no usable approver id.")
            }
            try keys.setCredential(id, for: key)
            enrolled = true
            // A replaced key the daemon retired is gone from both. One it
            // still knows leaves its credential there: the alert gives the
            // command that removes it, id and all. A key that was never
            // enrolled leaves nothing to remove.
            let retired = Set(ok.retired ?? [])
            var removals: [String] = []
            var retiredHere = 0
            for old in older where old.handle != key.handle {
                if (try? keys.delete(old)) != nil, let gone = old.credential, gone != id {
                    if retired.contains(gone) {
                        retiredHere += 1
                    } else {
                        removals.append(daemonCommand(list.terminal, account: account, verb: "remove " + gone))
                    }
                }
            }
            return Notices.enrolled(name: Text.clean(ok.name ?? label, max: 64), removals: removals, retired: retiredHere)
        }
    }

    func askCode(_ command: String) throws -> String {
        var problem: String?
        for _ in 0..<5 {
            guard let typed = ui.enrollCode(command: command, problem: problem) else {
                throw ApproverError.cancelled("Not enrolled: cancelled.")
            }
            let normalized: String
            do {
                normalized = try Flows.typedCode(typed)
            } catch {
                problem = CodeProblem.notASCII
                continue
            }
            if Messages.isEnrollCode(normalized) {
                return normalized
            }
            problem = CodeProblem.notACode
        }
        throw ApproverError.refused("Not enrolled: no valid enrollment code was typed.")
    }

    /// `typedCode` is a code as the owner typed or pasted it, in the form
    /// the daemon parses (store.NormalizeEnrollCode, mirrored by
    /// Messages.normalizeEnrollCode): any case, with or without the dashes,
    /// spaces and tabs it is printed or typed with. A line break pasted
    /// around it from a terminal is dropped too; the helper sends and signs
    /// the normalized code, so the daemon never sees one.
    static func typedCode(_ typed: String) throws -> String {
        try Messages.normalizeEnrollCode(typed.trimmingCharacters(in: CharacterSet(charactersIn: " \t\r\n")))
    }

    /// `daemonCommand` is a `seamlessd approvers` command run as the daemon's
    /// account, as `seam approve` would run the daemon's seamlessd
    /// (cmd/seam/approve.go sudoArgv): "enroll-code touchid" mints a Touch ID
    /// code, "remove <id>" removes a credential.
    func daemonCommand(_ t: Terminal?, account: String, verb: String) -> String {
        var parts = ["sudo -k -u", account]
        if let config = t?.config, config.hasPrefix("/") {
            parts.append("env SEAMLESS_CONFIG=" + Text.clean(config, max: 120))
        }
        if let exe = t?.seamlessd, exe.hasPrefix("/") {
            parts.append(Text.clean(exe, max: 120))
        } else {
            parts.append("seamlessd")
        }
        parts.append("approvers " + verb)
        return parts.joined(separator: " ")
    }

    // MARK: forget

    /// `forget` deletes this Mac's key after the owner confirms. The daemon
    /// keeps the credential until the daemon's account removes it.
    public func forget() -> Outcome {
        Flows.run(.forget) {
            try keys.checkAvailable()
            let stored = try keys.keys()
            if stored.isEmpty {
                return Notices.noKey()
            }
            let ids = stored.compactMap { $0.credential }
            guard ui.confirmForget(credentials: ids) else {
                throw ApproverError.cancelled("Nothing was deleted.")
            }
            for k in stored {
                try keys.delete(k)
            }
            return Notices.keyDeleted(credentials: ids)
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
            // seam has no forget command; a new enrollment deletes every older
            // key once the new one is enrolled (enroll, below).
            throw ApproverError.keyStore("Several Touch ID approver keys are stored on this Mac: run seam touchid enroll again, which keeps only the new key.")
        }
    }

    func decode<T: Decodable>(_ r: HTTPResponse, what: String) throws -> T {
        guard r.status == 200 else {
            if let refusal = try? JSONDecoder().decode(DaemonRefusal.self, from: r.body), let e = refusal.error, !e.isEmpty {
                throw ApproverError.daemon(Text.sentence("The daemon refused: " + Text.clean(e, max: 300)))
            }
            throw ApproverError.daemon("The daemon answered \(r.status) for \(what).")
        }
        do {
            return try JSONDecoder().decode(T.self, from: r.body)
        } catch {
            throw ApproverError.daemon("The daemon's answer for \(what) is not the JSON this helper reads.")
        }
    }
}

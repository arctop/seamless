import CryptoKit
import Foundation
import LocalAuthentication
import Security

/// The approver key: a P-256 key made by and kept in the Secure Enclave,
/// stored in the data protection keychain under the access group
/// "<team id>.com.arctop.seamless.approve".
///
/// Why the keychain and not a CryptoKit dataRepresentation file: a Secure
/// Enclave key blob is bound to this Mac, not to this app, so any process of
/// the owner's account could load a file-stored key, raise a Touch ID prompt
/// in its own words, and sign a real approval. A keychain access group is
/// claimed only by code signed with an entitlement that the team's
/// provisioning profile authorizes (Contents/embedded.provisionprofile), so
/// only the signed helper can reach the key.
///
/// The group is read from this process's own signature (its entitlements),
/// never written in source: the team id is substituted at build time.
public final class SecureEnclaveKeyStore: KeyStore {
    static let tag = Data("com.arctop.seamless.approve.touchid".utf8)
    /// The item's label carries the approver id the daemon returned.
    static let enrolledPrefix = "seamless approver "
    static let pendingLabel = "seamless approver (not enrolled)"

    let group: String?
    let problem: String?

    public init(bundleID: String? = Bundle.main.bundleIdentifier) {
        (group, problem) = Self.accessGroup(bundleID: bundleID)
    }

    static let unsigned = "This copy of Seamless Approve is not signed with its keychain entitlement and provisioning profile, so it has no key store. Install the signed release with seam touchid enroll."

    /// `accessGroup` is "<team>.<bundle id>" when this process is signed
    /// with that keychain access group and its team identifier, else why not.
    static func accessGroup(bundleID: String?) -> (String?, String?) {
        guard let bundleID = bundleID, !bundleID.isEmpty else {
            return (nil, "Seamless Approve is not running from its app bundle, so it has no key store.")
        }
        guard let task = SecTaskCreateFromSelf(nil) else {
            return (nil, unsigned)
        }
        let team = SecTaskCopyValueForEntitlement(task, "com.apple.developer.team-identifier" as CFString, nil) as? String
        let groups = SecTaskCopyValueForEntitlement(task, "keychain-access-groups" as CFString, nil) as? [String]
        guard let team = team, !team.isEmpty, let groups = groups else {
            return (nil, unsigned)
        }
        let want = team + "." + bundleID
        guard groups.contains(want) else {
            return (nil, "This copy of Seamless Approve is signed without its own keychain access group, so it has no key store.")
        }
        return (want, nil)
    }

    public func checkAvailable() throws {
        if let p = problem {
            throw ApproverError.keyStore(p)
        }
        guard SecureEnclave.isAvailable else {
            throw ApproverError.keyStore("This Mac has no Secure Enclave, so it cannot hold a Touch ID approver key.")
        }
    }

    func base() -> [String: Any] {
        [
            kSecClass as String: kSecClassKey,
            kSecAttrKeyClass as String: kSecAttrKeyClassPrivate,
            kSecAttrApplicationTag as String: Self.tag,
            kSecAttrAccessGroup as String: group ?? "",
            kSecUseDataProtectionKeychain as String: true,
        ]
    }

    public func keys() throws -> [StoredKey] {
        try checkAvailable()
        var q = base()
        q[kSecMatchLimit as String] = kSecMatchLimitAll
        q[kSecReturnAttributes as String] = true
        var out: CFTypeRef?
        let st = SecItemCopyMatching(q as CFDictionary, &out)
        if st == errSecItemNotFound {
            return []
        }
        guard st == errSecSuccess else {
            throw Self.failure("read the keychain", st)
        }
        let rows = out as? [[String: Any]] ?? []
        return rows.compactMap { row in
            guard let handle = row[kSecAttrApplicationLabel as String] as? Data else { return nil }
            let label = row[kSecAttrLabel as String] as? String ?? ""
            var credential: String?
            if label.hasPrefix(Self.enrolledPrefix) {
                let id = String(label.dropFirst(Self.enrolledPrefix.count))
                if Validate.isCredentialID(id) {
                    credential = id
                }
            }
            return StoredKey(handle: handle, credential: credential)
        }
    }

    public func create() throws -> (StoredKey, Data) {
        try checkAvailable()
        var cfError: Unmanaged<CFError>?
        // Every signature needs the owner's presence: Touch ID, an Apple
        // Watch, or the login password. ThisDeviceOnly: never synced, never
        // restored onto another Mac (a Secure Enclave key cannot leave it
        // anyway).
        guard let access = SecAccessControlCreateWithFlags(kCFAllocatorDefault, kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
                                                           [.privateKeyUsage, .userPresence], &cfError) else {
            throw Self.failure("describe the key's access control", cfError)
        }
        let attrs: [String: Any] = [
            kSecAttrKeyType as String: kSecAttrKeyTypeECSECPrimeRandom,
            kSecAttrKeySizeInBits as String: 256,
            kSecAttrTokenID as String: kSecAttrTokenIDSecureEnclave,
            kSecUseDataProtectionKeychain as String: true,
            kSecAttrAccessGroup as String: group ?? "",
            kSecPrivateKeyAttrs as String: [
                kSecAttrIsPermanent as String: true,
                kSecAttrApplicationTag as String: Self.tag,
                kSecAttrLabel as String: Self.pendingLabel,
                kSecAttrAccessControl as String: access,
                kSecAttrAccessGroup as String: group ?? "",
                kSecAttrSynchronizable as String: false,
            ] as [String: Any],
        ]
        guard let priv = SecKeyCreateRandomKey(attrs as CFDictionary, &cfError) else {
            throw Self.failure("create the key in the Secure Enclave", cfError)
        }
        guard let pub = SecKeyCopyPublicKey(priv),
              let raw = SecKeyCopyExternalRepresentation(pub, &cfError) as Data?,
              raw.count == 65, raw.first == 0x04 else {
            throw ApproverError.keyStore("The new key's public half is not a P-256 point.")
        }
        guard let attributes = SecKeyCopyAttributes(priv) as? [String: Any],
              let handle = attributes[kSecAttrApplicationLabel as String] as? Data else {
            throw ApproverError.keyStore("The new key carries no keychain label.")
        }
        // Confirm the key landed where the next run looks for it.
        guard try keys().contains(where: { $0.handle == handle }) else {
            throw ApproverError.keyStore("The new key was not stored in Seamless Approve's keychain group.")
        }
        return (StoredKey(handle: handle, credential: nil), raw)
    }

    public func sign(_ message: Data, with key: StoredKey, reason: String) throws -> Data {
        try checkAvailable()
        // A fresh context per signature: no reuse of an earlier unlock
        // (touchIDAuthenticationAllowableReuseDuration stays 0), and the
        // prompt says this approval's facts.
        let context = LAContext()
        context.localizedReason = reason
        return try sign(message, with: key, context: context)
    }

    /// `signAll` signs with one fresh context for the whole run, so the
    /// system can let the owner's presence, checked for the first key, cover
    /// the others rather than asking once per key; where it asks again, it
    /// asks in the same words. Still no earlier unlock is reused
    /// (touchIDAuthenticationAllowableReuseDuration stays 0).
    public func signAll(_ signings: [Signing], reason: String) throws -> [Data?] {
        try checkAvailable()
        let context = LAContext()
        context.localizedReason = reason
        var out: [Data?] = []
        for (i, s) in signings.enumerated() {
            do {
                out.append(try sign(s.message, with: s.key, context: context))
            } catch {
                if i == 0 {
                    throw error
                }
                out.append(nil)
            }
        }
        return out
    }

    func sign(_ message: Data, with key: StoredKey, context: LAContext) throws -> Data {
        var q = base()
        q[kSecAttrApplicationLabel as String] = key.handle
        q[kSecReturnRef as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        q[kSecUseAuthenticationContext as String] = context
        var out: CFTypeRef?
        let st = SecItemCopyMatching(q as CFDictionary, &out)
        if st == errSecItemNotFound {
            throw ApproverError.keyStore("The approver key is no longer in the keychain: run seam touchid enroll.")
        }
        guard st == errSecSuccess, let ref = out, CFGetTypeID(ref) == SecKeyGetTypeID() else {
            throw Self.failure("find the approver key", st)
        }
        let secKey = ref as! SecKey // the type id was checked above
        var cfError: Unmanaged<CFError>?
        guard let signature = SecKeyCreateSignature(secKey, .ecdsaSignatureMessageX962SHA256, message as CFData, &cfError) as Data? else {
            let e = cfError?.takeRetainedValue() as Error? as NSError?
            if let e = e, Self.isCancel(e) {
                throw ApproverError.cancelled("The Touch ID prompt was cancelled.")
            }
            throw ApproverError.keyStore("Touch ID did not sign: " + Self.describe(e))
        }
        return signature
    }

    public func setCredential(_ id: String, for key: StoredKey) throws {
        try checkAvailable()
        var q = base()
        q[kSecAttrApplicationLabel as String] = key.handle
        let st = SecItemUpdate(q as CFDictionary, [kSecAttrLabel as String: Self.enrolledPrefix + id] as CFDictionary)
        guard st == errSecSuccess else {
            throw Self.failure("record the approver id with the key", st)
        }
    }

    public func delete(_ key: StoredKey) throws {
        try checkAvailable()
        var q = base()
        q[kSecAttrApplicationLabel as String] = key.handle
        let st = SecItemDelete(q as CFDictionary)
        guard st == errSecSuccess || st == errSecItemNotFound else {
            throw Self.failure("delete the key", st)
        }
    }

    // MARK: errors

    static func isCancel(_ e: NSError) -> Bool {
        if e.domain == LAErrorDomain {
            return e.code == LAError.Code.userCancel.rawValue || e.code == LAError.Code.appCancel.rawValue
        }
        return e.domain == NSOSStatusErrorDomain && e.code == Int(errSecUserCanceled)
    }

    static func describe(_ e: NSError?) -> String {
        guard let e = e else { return "an unknown error" }
        if e.domain == LAErrorDomain {
            switch e.code {
            case LAError.Code.authenticationFailed.rawValue:
                return "Touch ID or the password was not accepted."
            case LAError.Code.systemCancel.rawValue:
                return "the system closed the prompt (the screen locked, or another prompt took its place)."
            case LAError.Code.biometryLockout.rawValue:
                return "Touch ID is locked after too many tries: unlock the Mac with its password, then try again."
            case LAError.Code.notInteractive.rawValue:
                return "no one is at this Mac's screen to answer the prompt."
            default:
                break
            }
        }
        if e.domain == NSOSStatusErrorDomain {
            return statusText(OSStatus(e.code))
        }
        return Text.clean(e.localizedDescription, max: 200)
    }

    static func statusText(_ st: OSStatus) -> String {
        switch st {
        case errSecMissingEntitlement:
            return unsigned
        case errSecInteractionNotAllowed:
            return "the keychain cannot ask for Touch ID now (is the Mac locked?)."
        case errSecAuthFailed:
            return "Touch ID or the password was not accepted."
        default:
            let text = SecCopyErrorMessageString(st, nil) as String? ?? "OSStatus \(st)"
            return Text.clean(text, max: 200) + " (\(st))"
        }
    }

    static func failure(_ what: String, _ st: OSStatus) -> ApproverError {
        if st == errSecMissingEntitlement {
            return .keyStore(unsigned)
        }
        return .keyStore("Could not \(what): " + statusText(st))
    }

    static func failure(_ what: String, _ cfError: Unmanaged<CFError>?) -> ApproverError {
        let e = cfError?.takeRetainedValue() as Error? as NSError?
        if let e = e, e.domain == NSOSStatusErrorDomain, OSStatus(e.code) == errSecMissingEntitlement {
            return .keyStore(unsigned)
        }
        return .keyStore("Could not \(what): " + describe(e))
    }
}

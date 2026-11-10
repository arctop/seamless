import CryptoKit
import Darwin
import Foundation
@testable import SeamApproveCore
import Security
import XCTest

/// A stand-in daemon on a real Unix socket: it answers each connection with
/// the next canned reply and records what it was sent. It runs as the test's
/// own uid, so a test that wants the exchange to go through admits that uid;
/// the production PeerGuard refuses it (PeerTests).
final class TestServer {
    let path: String
    private let fd: Int32
    private let lock = NSLock()
    private var received: [Data] = []
    private let replies: [Data]
    private let done = DispatchSemaphore(value: 0)

    init(replies: [Data]) throws {
        self.replies = replies
        path = NSTemporaryDirectory() + "sa-" + String(UUID().uuidString.prefix(8)) + ".sock"
        XCTAssertNil(Validate.socketPathProblem(path), path)
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        let bytes = Array(path.utf8)
        withUnsafeMutableBytes(of: &addr.sun_path) { $0.copyBytes(from: bytes) }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard rc == 0, listen(fd, 8) == 0 else {
            throw ApproverError.transport("test server: bind/listen failed: \(String(cString: strerror(errno)))")
        }
        let listener = fd
        let replies = self.replies
        Thread.detachNewThread { [self] in
            for reply in replies {
                let c = accept(listener, nil, nil)
                if c < 0 { break }
                // A reply to a client that has gone must fail a test, not kill it.
                var on: Int32 = 1
                _ = setsockopt(c, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
                let req = TestServer.readRequest(c)
                if req == TestServer.stopWord {
                    close(c)
                    break
                }
                lock.lock()
                received.append(req)
                lock.unlock()
                if !req.isEmpty {
                    reply.withUnsafeBytes { _ = send(c, $0.baseAddress, $0.count, 0) }
                }
                close(c)
            }
            done.signal()
        }
    }

    /// What `stop`'s connection says, which no request does. A client that
    /// connected and closed without a byte is a request to record (PeerTests),
    /// and accept may only now be returning it: connections come off the
    /// backlog in order, so it is still recorded, ahead of this one.
    private static let stopWord = Data("stop-test-server".utf8)

    /// `stop` wakes the accept loop with one last connection and removes the
    /// socket. Every test that makes a server calls it.
    func stop() {
        let c = socket(AF_UNIX, SOCK_STREAM, 0)
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        let bytes = Array(path.utf8)
        withUnsafeMutableBytes(of: &addr.sun_path) { $0.copyBytes(from: bytes) }
        _ = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(c, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        TestServer.stopWord.withUnsafeBytes { _ = send(c, $0.baseAddress, $0.count, 0) }
        close(c)
        _ = done.wait(timeout: .now() + 5)
        close(fd)
        unlink(path)
    }

    var requests: [Data] {
        lock.lock()
        defer { lock.unlock() }
        return received
    }

    /// `requestStrings` are the requests received, as text.
    var requestStrings: [String] {
        requests.map { String(decoding: $0, as: UTF8.self) }
    }

    /// Reads one request: headers, then Content-Length bytes of body. A
    /// client that closes without sending anything yields empty data.
    static func readRequest(_ c: Int32) -> Data {
        var out = Data()
        var buf = [UInt8](repeating: 0, count: 4096)
        while true {
            if let end = out.range(of: Data("\r\n\r\n".utf8)) {
                let head = String(decoding: out[..<end.lowerBound], as: UTF8.self).lowercased()
                var want = 0
                for line in head.components(separatedBy: "\r\n") where line.hasPrefix("content-length:") {
                    want = Int(line.dropFirst("content-length:".count).trimmingCharacters(in: .whitespaces)) ?? 0
                }
                if out.count - end.upperBound >= want {
                    return out
                }
            }
            let n = recv(c, &buf, buf.count, 0)
            if n <= 0 { return out }
            out.append(contentsOf: buf[0..<n])
        }
    }

    static func reply(_ status: Int, _ json: String) -> Data {
        let body = Data(json.utf8)
        var raw = Data("HTTP/1.0 \(status) X\r\nContent-Type: application/json\r\nContent-Length: \(body.count)\r\n\r\n".utf8)
        raw.append(body)
        return raw
    }
}

/// The form of a recorded POST, decoded.
func formOf(_ request: String) -> [String: String] {
    guard let r = request.range(of: "\r\n\r\n") else { return [:] }
    var c = URLComponents()
    c.percentEncodedQuery = String(request[r.upperBound...])
    var out: [String: String] = [:]
    for item in c.queryItems ?? [] {
        out[item.name] = item.value ?? ""
    }
    return out
}

/// A software P-256 key store with the Secure Enclave store's contract. The
/// keys are never permanent: no keychain is touched.
final class SoftwareKeyStore: KeyStore {
    var unavailable: String?
    var cancelSigning = false
    /// `cancelLater` cancels every signature of a signAll but the first, as
    /// an owner who cancels a second prompt would.
    var cancelLater = false
    private(set) var stored: [(StoredKey, SecKey)] = []
    /// `reasons` holds one entry per prompt: a sign, or a whole signAll (one
    /// check covering every key, as one shared context does).
    private(set) var reasons: [String] = []

    func checkAvailable() throws {
        if let u = unavailable {
            throw ApproverError.keyStore(u)
        }
    }

    func keys() throws -> [StoredKey] {
        stored.map { $0.0 }
    }

    func create() throws -> (StoredKey, Data) {
        let attrs: [String: Any] = [kSecAttrKeyType as String: kSecAttrKeyTypeECSECPrimeRandom, kSecAttrKeySizeInBits as String: 256]
        var err: Unmanaged<CFError>?
        guard let k = SecKeyCreateRandomKey(attrs as CFDictionary, &err) else {
            throw ApproverError.keyStore("software key: \(err!.takeRetainedValue())")
        }
        let pub = SecKeyCopyExternalRepresentation(SecKeyCopyPublicKey(k)!, nil)! as Data
        let key = StoredKey(handle: Data(Insecure.SHA1.hash(data: pub)), credential: nil)
        stored.append((key, k))
        return (key, pub)
    }

    /// `add` stores an enrolled key, as an earlier enrollment would have.
    @discardableResult
    func add(credential: String) throws -> StoredKey {
        let (key, _) = try create()
        try setCredential(credential, for: key)
        return try keys().first { $0.handle == key.handle }!
    }

    func sign(_ message: Data, with key: StoredKey, reason: String) throws -> Data {
        reasons.append(reason)
        return try signQuietly(message, with: key)
    }

    func signAll(_ signings: [Signing], reason: String) throws -> [Data?] {
        reasons.append(reason)
        var out: [Data?] = []
        for (i, s) in signings.enumerated() {
            if i > 0 && cancelLater {
                out.append(nil)
                continue
            }
            do {
                out.append(try signQuietly(s.message, with: s.key))
            } catch {
                if i == 0 {
                    throw error
                }
                out.append(nil)
            }
        }
        return out
    }

    func signQuietly(_ message: Data, with key: StoredKey) throws -> Data {
        if cancelSigning {
            throw ApproverError.cancelled("cancelled")
        }
        guard let k = secKey(key.handle) else {
            throw ApproverError.keyStore("no such key")
        }
        var err: Unmanaged<CFError>?
        guard let sig = SecKeyCreateSignature(k, .ecdsaSignatureMessageX962SHA256, message as CFData, &err) as Data? else {
            throw ApproverError.keyStore("sign: \(err!.takeRetainedValue())")
        }
        return sig
    }

    func setCredential(_ id: String, for key: StoredKey) throws {
        guard let i = stored.firstIndex(where: { $0.0.handle == key.handle }) else { throw ApproverError.keyStore("no such key") }
        stored[i].0 = StoredKey(handle: key.handle, credential: id)
    }

    func delete(_ key: StoredKey) throws {
        stored.removeAll { $0.0.handle == key.handle }
    }

    func secKey(_ handle: Data) -> SecKey? {
        stored.first { $0.0.handle == handle }?.1
    }

    /// `verifies` checks a DER signature over message with the public half of
    /// the key `handle` names.
    func verifies(_ signature: Data, _ message: Data, handle: Data) -> Bool {
        guard let k = secKey(handle), let pub = SecKeyCopyPublicKey(k) else { return false }
        return SecKeyVerifySignature(pub, .ecdsaSignatureMessageX962SHA256, message as CFData, signature as CFData, nil)
    }

    func publicKey(_ handle: Data) -> Data? {
        guard let k = secKey(handle), let pub = SecKeyCopyPublicKey(k) else { return nil }
        return SecKeyCopyExternalRepresentation(pub, nil) as Data?
    }
}

final class FakeUI: ApproverUI {
    var codes: [String?]
    var forget = true
    private(set) var asked: [(command: String, problem: String?)] = []
    private(set) var forgetAsked: [[String]] = []

    init(codes: [String?] = []) {
        self.codes = codes
    }

    func enrollCode(command: String, key: String, problem: String?) -> String? {
        asked.append((command, problem))
        return codes.isEmpty ? nil : codes.removeFirst()
    }

    func confirmForget(credentials: [String]) -> Bool {
        forgetAsked.append(credentials)
        return forget
    }
}

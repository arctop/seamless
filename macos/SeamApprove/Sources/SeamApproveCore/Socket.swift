import Darwin
import Foundation

/// One request and its reply over a connection, with the uid the kernel
/// reported for the process at the other end.
public struct Exchange {
    public let response: HTTPResponse
    public let peerUID: uid_t
}

/// How the flows reach the daemon. Production is UnixSocketTransport; a test
/// can stand a server up on its own socket.
public protocol Transport {
    func exchange(_ request: HTTPRequest) throws -> Exchange
}

/// `PeerGuard` is the check every connection passes right after connect,
/// before a byte is sent: the socket's peer must not run as the helper's own
/// uid. The helper runs as the owner, and so may every agent the owner runs;
/// any of them could listen on a socket and pass its path here, but none of
/// them can be another uid. The daemon runs as its own account. The first
/// peer is pinned, so every connection of one run reaches the same account;
/// the flows also check that account is the one the daemon names
/// (`requireAccount`).
public final class PeerGuard {
    public let ownUID: uid_t
    public private(set) var pinned: uid_t?

    public init(ownUID: uid_t) {
        self.ownUID = ownUID
    }

    public func admit(_ peer: uid_t) throws {
        if peer == ownUID {
            throw ApproverError.peer("the process serving the socket runs as this account (uid \(peer)), not as the daemon's: refusing to talk to it")
        }
        if let p = pinned, p != peer {
            throw ApproverError.peer("the socket's peer changed from uid \(p) to uid \(peer) during one approval: refusing to go on")
        }
        pinned = peer
    }
}

/// `requireAccount` refuses unless `account` -- the account the daemon names
/// as its own -- resolves to the uid the kernel reported for the socket's
/// peer. `lookup` resolves an account name (uidOfAccount in production).
public func requireAccount(_ account: String, isPeer peer: uid_t, lookup: (String) -> uid_t?) throws {
    guard let uid = lookup(account) else {
        throw ApproverError.peer("the daemon names \(Text.quoted(account)) as its account, and this Mac has no such account")
    }
    guard uid == peer else {
        throw ApproverError.peer("the socket is served by uid \(peer), but the daemon names \(Text.quoted(account)) (uid \(uid)) as its account: refusing to go on")
    }
}

/// `uidOfAccount` is getpwnam_r's uid for name, or the number of a "#uid"
/// (sudo's spelling of a bare uid).
public func uidOfAccount(_ name: String) -> uid_t? {
    if name.hasPrefix("#") {
        return UInt32(name.dropFirst())
    }
    var pwd = passwd()
    var result: UnsafeMutablePointer<passwd>?
    var buf = [CChar](repeating: 0, count: 16 * 1024)
    let rc = getpwnam_r(name, &pwd, &buf, buf.count, &result)
    guard rc == 0, result != nil else { return nil }
    return pwd.pw_uid
}

/// HTTP over the peer_auth Unix socket, one connection per request.
public final class UnixSocketTransport: Transport {
    public let path: String
    let admit: (uid_t) throws -> Void
    let timeoutSeconds: Int
    let maxResponseBytes: Int
    let userAgent: String

    /// `admit` judges the peer's uid before anything is sent (PeerGuard.admit
    /// in production).
    public init(path: String, userAgent: String, timeoutSeconds: Int = 30, maxResponseBytes: Int = 1 << 20,
                admit: @escaping (uid_t) throws -> Void) {
        self.path = path
        self.userAgent = userAgent
        self.timeoutSeconds = timeoutSeconds
        self.maxResponseBytes = maxResponseBytes
        self.admit = admit
    }

    public func exchange(_ request: HTTPRequest) throws -> Exchange {
        if let problem = Validate.socketPathProblem(path) {
            throw ApproverError.transport(problem)
        }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw ApproverError.transport("cannot open a socket: \(String(cString: strerror(errno)))")
        }
        defer { close(fd) }
        var on: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        var tv = timeval(tv_sec: timeoutSeconds, tv_usec: 0)
        _ = setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        _ = setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        let bytes = Array(path.utf8)
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: bytes)
        }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard rc == 0 else {
            let e = errno
            switch e {
            case ENOENT:
                throw ApproverError.transport("there is no daemon socket at \(Text.quoted(path, max: 104))")
            case ECONNREFUSED:
                throw ApproverError.transport("nothing is listening on \(Text.quoted(path, max: 104)): is seamlessd running?")
            case EACCES:
                throw ApproverError.transport("this account may not open \(Text.quoted(path, max: 104))")
            default:
                throw ApproverError.transport("cannot connect to \(Text.quoted(path, max: 104)): \(String(cString: strerror(e)))")
            }
        }

        var uid: uid_t = 0
        var gid: gid_t = 0
        guard getpeereid(fd, &uid, &gid) == 0 else {
            throw ApproverError.peer("the kernel did not report who serves \(Text.quoted(path, max: 104)): \(String(cString: strerror(errno)))")
        }
        try admit(uid)

        try sendAll(fd, request.serialized(userAgent: userAgent))
        let raw = try receiveAll(fd)
        do {
            return Exchange(response: try HTTPParser.parse(raw), peerUID: uid)
        } catch let e as HTTPError {
            throw ApproverError.daemon(e.description)
        }
    }

    func sendAll(_ fd: Int32, _ data: Data) throws {
        try data.withUnsafeBytes { (buf: UnsafeRawBufferPointer) in
            var off = 0
            while off < buf.count {
                let n = send(fd, buf.baseAddress!.advanced(by: off), buf.count - off, 0)
                if n < 0 {
                    if errno == EINTR { continue }
                    if errno == EAGAIN || errno == EWOULDBLOCK {
                        throw ApproverError.transport("the daemon stopped reading the request (timed out)")
                    }
                    throw ApproverError.transport("sending to the daemon failed: \(String(cString: strerror(errno)))")
                }
                off += n
            }
        }
    }

    func receiveAll(_ fd: Int32) throws -> Data {
        var out = Data()
        var buf = [UInt8](repeating: 0, count: 16 * 1024)
        while true {
            let n = recv(fd, &buf, buf.count, 0)
            if n == 0 {
                return out
            }
            if n < 0 {
                if errno == EINTR { continue }
                if errno == EAGAIN || errno == EWOULDBLOCK {
                    throw ApproverError.transport("the daemon did not answer within \(timeoutSeconds) seconds")
                }
                throw ApproverError.transport("reading the daemon's answer failed: \(String(cString: strerror(errno)))")
            }
            out.append(contentsOf: buf[0..<n])
            if out.count > maxResponseBytes {
                throw ApproverError.daemon("the daemon's answer is larger than \(maxResponseBytes) bytes")
            }
        }
    }
}

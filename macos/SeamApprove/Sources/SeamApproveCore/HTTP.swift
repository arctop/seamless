import Foundation

// A minimal HTTP client half for one request per connection over the
// peer_auth Unix socket.
//
// The request is HTTP/1.0. Go's server never chunks an HTTP/1.0 reply: it
// sends Content-Length when the handler's body fits its buffer and otherwise
// ends the body by closing the connection. An HTTP/1.1 request with
// "Connection: close" is not enough -- Go still chunks a body larger than its
// 2 KiB buffer then (measured against net/http, go1.26). The parser decodes
// chunked bodies anyway, so a server that chunks regardless still works.

/// One request: a method, a path with its query, and an optional form.
public struct HTTPRequest: Equatable {
    public var method: String
    public var path: String
    public var form: [FormField]?

    public init(method: String, path: String, form: [FormField]? = nil) {
        self.method = method
        self.path = path
        self.form = form
    }

    /// `serialized` is the request on the wire. Host is "localhost": the
    /// socket, not a name, says which daemon this is.
    public func serialized(userAgent: String) -> Data {
        var head = "\(method) \(path) HTTP/1.0\r\n"
        head += "Host: localhost\r\n"
        head += "User-Agent: \(userAgent)\r\n"
        head += "Accept: application/json\r\n"
        head += "Connection: close\r\n"
        var body = Data()
        if let form = form {
            body = Data(Form.encode(form).utf8)
            head += "Content-Type: application/x-www-form-urlencoded\r\n"
        }
        if form != nil || method == "POST" {
            head += "Content-Length: \(body.count)\r\n"
        }
        head += "\r\n"
        var out = Data(head.utf8)
        out.append(body)
        return out
    }
}

/// One form field; order is kept.
public struct FormField: Equatable {
    public var name: String
    public var value: String

    public init(_ name: String, _ value: String) {
        self.name = name
        self.value = value
    }
}

public enum Form {
    /// `encode` is application/x-www-form-urlencoded with every byte outside
    /// the unreserved set percent-encoded, '+', '/' and '=' of base64
    /// included.
    public static func encode(_ fields: [FormField]) -> String {
        fields.map { escape($0.name) + "=" + escape($0.value) }.joined(separator: "&")
    }

    static func escape(_ s: String) -> String {
        var out = ""
        for b in s.utf8 {
            switch b {
            case UInt8(ascii: "A")...UInt8(ascii: "Z"), UInt8(ascii: "a")...UInt8(ascii: "z"),
                 UInt8(ascii: "0")...UInt8(ascii: "9"), UInt8(ascii: "-"), UInt8(ascii: "."),
                 UInt8(ascii: "_"), UInt8(ascii: "~"):
                out.append(Character(Unicode.Scalar(b)))
            default:
                out += String(format: "%%%02X", b)
            }
        }
        return out
    }
}

/// A parsed reply.
public struct HTTPResponse: Equatable {
    public let status: Int
    /// Header names lower-cased; repeated headers joined with ", ".
    public let headers: [String: String]
    public let body: Data
}

public struct HTTPError: Error, Equatable, CustomStringConvertible {
    public let description: String

    init(_ d: String) {
        description = d
    }
}

public enum HTTPParser {
    static let crlf = Data("\r\n".utf8)
    static let headEnd = Data("\r\n\r\n".utf8)

    /// `parse` reads a whole reply, read until the server closed the
    /// connection. The body is framed by Transfer-Encoding: chunked, else by
    /// Content-Length, else by the end of the stream.
    public static func parse(_ raw: Data) throws -> HTTPResponse {
        guard let end = raw.range(of: headEnd) else {
            throw HTTPError("the daemon's reply ended inside its headers")
        }
        guard let head = String(data: raw[raw.startIndex..<end.lowerBound], encoding: .utf8) else {
            throw HTTPError("the daemon's reply headers are not text")
        }
        var lines = head.components(separatedBy: "\r\n")
        let statusLine = lines.removeFirst()
        let parts = statusLine.split(separator: " ", maxSplits: 2, omittingEmptySubsequences: true)
        guard parts.count >= 2, parts[0].hasPrefix("HTTP/1."), parts[1].count == 3, let status = Int(parts[1]) else {
            throw HTTPError("the daemon's reply has no HTTP status line")
        }
        var headers: [String: String] = [:]
        for line in lines {
            guard let colon = line.firstIndex(of: ":") else {
                throw HTTPError("the daemon's reply has a malformed header")
            }
            let name = line[..<colon].trimmingCharacters(in: .whitespaces).lowercased()
            let value = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
            guard !name.isEmpty else {
                throw HTTPError("the daemon's reply has a malformed header")
            }
            headers[name] = headers[name].map { $0 + ", " + value } ?? value
        }
        let rest = raw[end.upperBound...]
        let body: Data
        if let te = headers["transfer-encoding"]?.lowercased(), te.contains("chunked") {
            body = try dechunk(Data(rest))
        } else if let cl = headers["content-length"] {
            let values = Set(cl.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) })
            guard values.count == 1, let n = Int(values.first!), n >= 0 else {
                throw HTTPError("the daemon's reply has an invalid Content-Length")
            }
            guard rest.count >= n else {
                throw HTTPError("the daemon's reply ended before its body did")
            }
            body = Data(rest.prefix(n))
        } else {
            body = Data(rest)
        }
        return HTTPResponse(status: status, headers: headers, body: body)
    }

    /// `dechunk` decodes a chunked body: hex size lines (extensions after ';'
    /// ignored), each chunk followed by CRLF, ended by a zero-size chunk
    /// (trailers ignored).
    static func dechunk(_ data: Data) throws -> Data {
        var out = Data()
        var i = data.startIndex
        while true {
            guard let lineEnd = data[i...].range(of: crlf) else {
                throw HTTPError("the daemon's chunked reply ended inside a chunk size")
            }
            guard let line = String(data: data[i..<lineEnd.lowerBound], encoding: .ascii) else {
                throw HTTPError("the daemon's chunked reply has a malformed chunk size")
            }
            let sizeText = line.split(separator: ";", maxSplits: 1, omittingEmptySubsequences: false).first.map {
                $0.trimmingCharacters(in: .whitespaces)
            } ?? ""
            guard !sizeText.isEmpty, sizeText.count <= 8, let size = Int(sizeText, radix: 16), size >= 0 else {
                throw HTTPError("the daemon's chunked reply has a malformed chunk size")
            }
            i = lineEnd.upperBound
            if size == 0 {
                return out
            }
            guard data.distance(from: i, to: data.endIndex) >= size + 2 else {
                throw HTTPError("the daemon's chunked reply ended inside a chunk")
            }
            let chunkEnd = data.index(i, offsetBy: size)
            out.append(data[i..<chunkEnd])
            guard data[chunkEnd..<data.index(chunkEnd, offsetBy: 2)] == crlf else {
                throw HTTPError("the daemon's chunked reply has a chunk without its CRLF")
            }
            i = data.index(chunkEnd, offsetBy: 2)
        }
    }
}

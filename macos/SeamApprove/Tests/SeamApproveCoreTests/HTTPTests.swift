import Foundation
@testable import SeamApproveCore
import XCTest

final class HTTPTests: XCTestCase {
    // The body of the replies below: what a Go handler writing
    // json.NewEncoder(w).Encode({"pad": 9000 x's}) sends, 9011 bytes.
    let body = Data(("{\"pad\":\"" + String(repeating: "x", count: 9000) + "\"}\n").utf8)

    // Captured from net/http (go1.26) over a Unix socket, 2026-11-06: an
    // HTTP/1.0 request gets no chunking and no Content-Length for a body
    // past the server's buffer -- the close ends it.
    func testGoReplyToHTTP10() throws {
        var raw = Data("HTTP/1.0 200 OK\r\nContent-Type: application/json\r\nDate: Sat, 07 Nov 2026 03:48:59 GMT\r\n\r\n".utf8)
        raw.append(body)
        let r = try HTTPParser.parse(raw)
        XCTAssertEqual(r.status, 200)
        XCTAssertEqual(r.headers["content-type"], "application/json")
        XCTAssertEqual(r.body, body)
    }

    // The same handler, an HTTP/1.1 request with Connection: close: Go
    // chunks anyway (one 0x2333-byte chunk), which is why the helper speaks
    // HTTP/1.0 -- and decodes chunks regardless.
    func testGoChunkedReplyToHTTP11() throws {
        var raw = Data("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nDate: Sat, 07 Nov 2026 03:48:59 GMT\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n2333\r\n".utf8)
        raw.append(body)
        raw.append(Data("\r\n0\r\n\r\n".utf8))
        XCTAssertEqual(body.count, 0x2333)
        let r = try HTTPParser.parse(raw)
        XCTAssertEqual(r.body, body)
    }

    func testChunkedManyChunksExtensionsAndTrailers() throws {
        let raw = Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4;ext=1\r\nWiki\r\n5\r\npedia\r\nE\r\n in\r\n\r\nchunks.\r\n0\r\nX-Trailer: y\r\n\r\n".utf8)
        XCTAssertEqual(String(data: try HTTPParser.parse(raw).body, encoding: .utf8), "Wikipedia in\r\n\r\nchunks.")
    }

    func testContentLength() throws {
        let raw = Data("HTTP/1.0 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"error\":\"x\"}trailing".utf8)
        let r = try HTTPParser.parse(raw)
        XCTAssertEqual(r.status, 404)
        XCTAssertEqual(String(data: r.body, encoding: .utf8), "{\"error\":\"x\"}")
    }

    func testMalformedReplies() {
        let bad = [
            "HTTP/1.0 200 OK\r\nContent-Length: 5\r\n",
            "SSH-2.0-OpenSSH\r\n\r\n",
            "HTTP/1.0 2000 OK\r\n\r\n",
            "HTTP/1.0 200 OK\r\nContent-Length: 10\r\n\r\nshort",
            "HTTP/1.0 200 OK\r\nContent-Length: 1, 2\r\n\r\nab",
            "HTTP/1.0 200 OK\r\nno colon here\r\n\r\n",
            "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nab\r\n0\r\n\r\n",
            "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n10\r\nshort\r\n",
            "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nabXX0\r\n\r\n",
            "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nab\r\n",
        ]
        for s in bad {
            XCTAssertThrowsError(try HTTPParser.parse(Data(s.utf8)), s)
        }
    }

    func testRequestOnTheWire() {
        let get = HTTPRequest(method: "GET", path: "/console/access/01JC7Q2ZK5V0M8W9X3Y4Z5A6B7?format=json")
        XCTAssertEqual(String(data: get.serialized(userAgent: "SeamlessApprove/1"), encoding: .utf8),
                       "GET /console/access/01JC7Q2ZK5V0M8W9X3Y4Z5A6B7?format=json HTTP/1.0\r\nHost: localhost\r\nUser-Agent: SeamlessApprove/1\r\nAccept: application/json\r\nConnection: close\r\n\r\n")
        let post = HTTPRequest(method: "POST", path: "/x", form: [FormField("signature", "MEUCIQ+/a=="), FormField("name", "Owner's Mac é")])
        XCTAssertEqual(String(data: post.serialized(userAgent: "u"), encoding: .utf8),
                       "POST /x HTTP/1.0\r\nHost: localhost\r\nUser-Agent: u\r\nAccept: application/json\r\nConnection: close\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 59\r\n\r\nsignature=MEUCIQ%2B%2Fa%3D%3D&name=Owner%27s%20Mac%20%C3%A9")
    }

    func testFormRoundTripsThroughURLComponents() {
        let value = "+/=&?# %\u{00E9}"
        let encoded = Form.encode([FormField("k", value)])
        var c = URLComponents()
        c.percentEncodedQuery = encoded
        XCTAssertEqual(c.queryItems?.first?.value, value)
    }
}

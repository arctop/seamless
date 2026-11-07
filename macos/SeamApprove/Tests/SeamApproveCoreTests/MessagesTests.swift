import Foundation
@testable import SeamApproveCore
import XCTest

// Golden vectors computed by the Go side's own functions
// (store.NormalizeEnrollCode, approval.TouchIDEnrollMessage,
// approval.TouchIDApproveMessage) on 2026-11-06. A change on either side that
// breaks byte equality fails here before it fails an owner's approval.
final class MessagesTests: XCTestCase {
    func testNormalizeEnrollCodeMatchesGo() throws {
        let cases: [(String, String)] = [
            ("abcd-efgh-jkmn-pqrs", "ABCDEFGHJKMNPQRS"),
            ("ABCD-EFGH-JKMN-PQRS", "ABCDEFGHJKMNPQRS"),
            (" o0il-IL10 \t xyz-9 ", "00111110XYZ9"),
            ("0123 4567\t89ab-cdef", "0123456789ABCDEF"),
            ("", ""),
        ]
        for (typed, want) in cases {
            XCTAssertEqual(try Messages.normalizeEnrollCode(typed), want, typed)
        }
    }

    func testNormalizeRefusesNonASCII() {
        // Go's strings.ToUpper maps U+0131 (dotless i) to 'I' and so to '1';
        // the helper refuses rather than fold any differently.
        XCTAssertThrowsError(try Messages.normalizeEnrollCode("abcd-\u{0131}fgh"))
        XCTAssertThrowsError(try Messages.normalizeEnrollCode("ABCD\u{00A0}EFGH"))
    }

    func testIsEnrollCode() throws {
        XCTAssertTrue(Messages.isEnrollCode(try Messages.normalizeEnrollCode("abcd-efgh-jkmn-pqrs")))
        XCTAssertFalse(Messages.isEnrollCode("ABCDEFGHJKMNPQR"), "15 symbols")
        XCTAssertFalse(Messages.isEnrollCode("ABCDEFGHJKMNPQRU"), "U is not Crockford")
        XCTAssertFalse(Messages.isEnrollCode(""))
    }

    static let publicKey: Data = {
        var d = Data([0x04])
        d.append(contentsOf: (1...64).map { UInt8($0) })
        return d
    }()

    func testEnrollMessageMatchesGo() throws {
        XCTAssertEqual(Self.publicKey.hex, "040102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40")
        XCTAssertEqual(
            try Messages.touchIDEnrollMessage(code: "abcd-efgh-jkmn-pqrs", publicKey: Self.publicKey).hex,
            "7365616d6c65737320746f756368696420656e726f6c6c6d656e7420763100f598056127fcd4387651a49b3a4235d8ce50d71f4091068df51511cfc10f388f040102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40")
        XCTAssertEqual(
            try Messages.touchIDEnrollMessage(code: "0ilo-OILO", publicKey: Self.publicKey).hex,
            "7365616d6c65737320746f756368696420656e726f6c6c6d656e74207631007b158f7df9bf300bcd68345e7e1f91df41d15bf7072323d6c78800e90c196548040102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40")
    }

    func testApproveMessageMatchesGo() {
        let challenge = Data((0..<32).map { UInt8(0xA0 + $0) })
        XCTAssertEqual(challenge.base64EncodedString(), "oKGio6SlpqeoqaqrrK2ur7CxsrO0tba3uLm6u7y9vr8=")
        XCTAssertEqual(
            Messages.touchIDApproveMessage(id: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7", challenge: challenge,
                                           digest: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
                                           access: "read-write", duration: "1h").hex,
            "7365616d6c65737320746f756368696420617070726f76616c2076310030314a433751325a4b3556304d385739583359345a354136423700a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf003966383664303831383834633764363539613266656161306335356164303135613362663466316232623062383232636431356436633135623066303061303800726561642d777269746500316800")
        XCTAssertEqual(
            Messages.touchIDApproveMessage(id: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7", challenge: Data([0, 1, 0, 0xFF]),
                                           digest: "d", access: "read-only", duration: "session").hex,
            "7365616d6c65737320746f756368696420617070726f76616c2076310030314a433751325a4b3556304d385739583359345a354136423700000100ff006400726561642d6f6e6c790073657373696f6e00")
    }
}

extension Data {
    var hex: String {
        map { String(format: "%02x", $0) }.joined()
    }
}

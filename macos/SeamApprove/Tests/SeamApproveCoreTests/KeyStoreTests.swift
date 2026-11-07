import Foundation
@testable import SeamApproveCore
import XCTest

/// The Secure Enclave store as an unsigned process sees it: the test runner
/// carries no keychain access group of ours, which is exactly an unsigned or
/// tampered copy of the helper. Every operation refuses before it reaches
/// the keychain, so no item is read or written and nothing prompts.
final class KeyStoreTests: XCTestCase {
    func testAnUnsignedProcessHasNoKeyStore() {
        let store = SecureEnclaveKeyStore(bundleID: "org.thereisnospoon.seamless.approve")
        XCTAssertNil(store.group)
        XCTAssertEqual(store.problem, SecureEnclaveKeyStore.unsigned)
        for (what, op) in [
            ("checkAvailable", { try store.checkAvailable() }),
            ("keys", { _ = try store.keys() }),
            ("create", { _ = try store.create() }),
            ("sign", { _ = try store.sign(Data("m".utf8), with: StoredKey(handle: Data([1]), credential: "x"), reason: "r") }),
            ("setCredential", { try store.setCredential("x", for: StoredKey(handle: Data([1]), credential: nil)) }),
            ("delete", { try store.delete(StoredKey(handle: Data([1]), credential: nil)) }),
        ] as [(String, () throws -> Void)] {
            XCTAssertThrowsError(try op(), what) { e in
                XCTAssertEqual(e as? ApproverError, .keyStore(SecureEnclaveKeyStore.unsigned), what)
            }
        }
    }

    func testOutsideItsBundle() {
        let (group, problem) = SecureEnclaveKeyStore.accessGroup(bundleID: nil)
        XCTAssertNil(group)
        XCTAssertNotNil(problem)
    }

    /// The whole approve run on an unsigned copy: it stops at the key store,
    /// before any socket is opened.
    func testApproveOnAnUnsignedCopy() {
        let flows = Flows(transport: UnixSocketTransport(path: "/nonexistent.sock", userAgent: "t", admit: { _ in XCTFail("no connection") }),
                          keys: SecureEnclaveKeyStore(bundleID: "org.thereisnospoon.seamless.approve"), ui: FakeUI())
        XCTAssertEqual(flows.approve(id: "01JC7Q2ZK5V0M8W9X3Y4Z5A6B7", duration: "1h"), .failed(SecureEnclaveKeyStore.unsigned))
        XCTAssertEqual(flows.enroll(name: nil), .failed(SecureEnclaveKeyStore.unsigned))
        XCTAssertEqual(flows.forget(), .failed(SecureEnclaveKeyStore.unsigned))
    }
}

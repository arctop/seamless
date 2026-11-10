import CryptoKit
import Foundation

/// The bytes the helper's key signs, byte for byte what the daemon verifies
/// (internal/approval: TouchIDApproveMessage in request.go,
/// TouchIDEnrollMessage and TouchIDReplaceMessage in enroll.go,
/// store.NormalizeEnrollCode in internal/store/approvers.go). The Secure
/// Enclave signs SHA-256 of them (ecdsaSignatureMessageX962SHA256), and the
/// daemon's VerifyTouchID hashes the same way.
public enum Messages {
    static let approveTag = "seamless touchid approval v1"
    static let enrollTag = "seamless touchid enrollment v1"
    static let replaceTag = "seamless touchid replacement v1"

    /// An enrollment code as typed that holds something other than ASCII. A
    /// minted code is 16 ASCII symbols, so such a code never matches one; the
    /// helper refuses it rather than fold it differently from Go's
    /// strings.ToUpper.
    public struct NonASCIICode: Error {}

    /// `normalizeEnrollCode` mirrors store.NormalizeEnrollCode: upper case, the
    /// dashes, spaces and tabs it is printed or typed with dropped, and
    /// Crockford's look-alikes folded (O to 0, I and L to 1).
    public static func normalizeEnrollCode(_ code: String) throws -> String {
        var out = String.UnicodeScalarView()
        for u in code.unicodeScalars {
            guard u.isASCII else { throw NonASCIICode() }
            var c = u
            if (0x61...0x7A).contains(c.value) {
                c = Unicode.Scalar(c.value - 0x20)!
            }
            switch c {
            case "-", " ", "\t":
                continue
            case "O":
                c = "0"
            case "I", "L":
                c = "1"
            default:
                break
            }
            out.append(c)
        }
        return String(out)
    }

    /// `isEnrollCode` reports whether a normalized code has the shape the
    /// daemon mints (16 Crockford symbols), so a typo is caught before the
    /// owner is asked for Touch ID. It does not change what is signed.
    public static func isEnrollCode(_ normalized: String) -> Bool {
        normalized.unicodeScalars.count == 16 && normalized.unicodeScalars.allSatisfy { Validate.crockford.contains($0) }
    }

    /// `keyFingerprint` is how the owner names a new key when minting its
    /// enrollment code (store.ApproverKeyFingerprint): SHA-256 of the raw
    /// public key, its first 80 bits as 16 Crockford symbols, printed
    /// XXXX-XXXX-XXXX-XXXX. A code is minted for this key alone, so one read
    /// off the owner's terminal cannot enroll any other.
    public static func keyFingerprint(publicKey: Data) -> String {
        let symbols = Array("0123456789ABCDEFGHJKMNPQRSTVWXYZ")
        var out = ""
        var written = 0
        var acc: UInt32 = 0
        var bits: UInt32 = 0
        for byte in SHA256.hash(data: publicKey) where written < 16 {
            acc = (acc << 8) | UInt32(byte)
            bits += 8
            while bits >= 5 && written < 16 {
                bits -= 5
                if written > 0 && written % 4 == 0 {
                    out.append("-")
                }
                out.append(symbols[Int((acc >> bits) & 31)])
                written += 1
            }
        }
        return out
    }

    /// TouchIDEnrollMessage(code, publicKey): the tag and a NUL, SHA-256 of
    /// the normalized code, then the raw public key.
    public static func touchIDEnrollMessage(code: String, publicKey: Data) throws -> Data {
        let normalized = try normalizeEnrollCode(code)
        var msg = Data(enrollTag.utf8)
        msg.append(0)
        msg.append(contentsOf: SHA256.hash(data: Data(normalized.utf8)))
        msg.append(publicKey)
        return msg
    }

    /// TouchIDReplaceMessage(code, publicKey, retiredID): what an earlier key
    /// of this Mac signs so that the enrollment of the new key `publicKey`
    /// retires its credential `retiredID` on the daemon. The tag and a NUL,
    /// SHA-256 of the normalized code, the new raw public key, then the id.
    public static func touchIDReplaceMessage(code: String, publicKey: Data, retiring retiredID: String) throws -> Data {
        let normalized = try normalizeEnrollCode(code)
        var msg = Data(replaceTag.utf8)
        msg.append(0)
        msg.append(contentsOf: SHA256.hash(data: Data(normalized.utf8)))
        msg.append(publicKey)
        msg.append(Data(retiredID.utf8))
        return msg
    }

    /// TouchIDApproveMessage(id, challenge, digest, access, duration): the tag
    /// and a NUL, then each part followed by a NUL. The challenge is its raw
    /// bytes, not its base64.
    public static func touchIDApproveMessage(id: String, challenge: Data, digest: String, access: String, duration: String) -> Data {
        var msg = Data(approveTag.utf8)
        msg.append(0)
        for part in [Data(id.utf8), challenge, Data(digest.utf8), Data(access.utf8), Data(duration.utf8)] {
            msg.append(part)
            msg.append(0)
        }
        return msg
    }
}

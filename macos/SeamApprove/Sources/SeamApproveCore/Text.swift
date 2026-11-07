import Foundation

/// Text the helper shows: every value from the daemon or from argv passes
/// through `clean` before it reaches a dialog, the Touch ID prompt, or stderr.
public enum Text {
    /// The longest `clean` result, in characters, when no cap is given.
    public static let defaultMax = 200

    /// `clean` makes a value safe to show on one line: it drops format
    /// characters (the bidi controls and the zero-width ones are format
    /// characters), private-use and unassigned code points, turns control
    /// characters, line and paragraph separators and every kind of space into
    /// one plain space, trims, and caps the result at `max` characters
    /// (`cap`).
    public static func clean(_ s: String, max: Int = defaultMax) -> String {
        var out = String.UnicodeScalarView()
        var space = false
        for u in s.unicodeScalars {
            switch u.properties.generalCategory {
            case .control, .lineSeparator, .paragraphSeparator, .spaceSeparator:
                space = !out.isEmpty
            case .format, .privateUse, .surrogate, .unassigned:
                continue
            default:
                if space {
                    out.append(" ")
                    space = false
                }
                out.append(u)
            }
        }
        return cap(String(out), max: max)
    }

    /// `cap` keeps at most `max` characters, and at most four code points per
    /// character on average, so a pile of combining marks cannot stretch one
    /// character across the prompt. A cut value ends in an ellipsis, which
    /// counts towards `max`.
    public static func cap(_ s: String, max: Int) -> String {
        guard max > 0 else { return "" }
        let budget = 4 * max
        var chars = 0
        var scalars = 0
        for ch in s {
            chars += 1
            scalars += ch.unicodeScalars.count
            if chars > max || scalars > budget {
                return cut(s, max: max - 1, budget: budget - 1) + "\u{2026}"
            }
        }
        return s
    }

    private static func cut(_ s: String, max: Int, budget: Int) -> String {
        var out = ""
        var chars = 0
        var scalars = 0
        for ch in s {
            let n = ch.unicodeScalars.count
            if chars + 1 > max || scalars + n > budget {
                break
            }
            out.append(ch)
            chars += 1
            scalars += n
        }
        while out.last == " " {
            out.removeLast()
        }
        return out
    }

    /// `quoted` is a cleaned value between typographic quotes, for naming an
    /// argument or a value in a message.
    public static func quoted(_ s: String, max: Int = 48) -> String {
        "\u{201C}" + clean(s, max: max) + "\u{201D}"
    }
}

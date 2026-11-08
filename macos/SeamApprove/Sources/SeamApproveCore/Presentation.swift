import Foundation

// What the helper's windows say, as pure functions of the facts, so the tests
// pin every word. The SeamApprove executable only renders these: AppUI turns
// a Notice into an NSAlert and a Prompt into a dialog.

/// `WallClock` says an instant as the owner reads a clock: in this Mac's time
/// zone, in its locale's format, with the day when it is not today. Tests pass
/// a fixed now, zone and locale.
public struct WallClock {
    public var now: () -> Date
    public var timeZone: TimeZone
    public var locale: Locale

    public init(now: @escaping () -> Date = Date.init, timeZone: TimeZone = .autoupdatingCurrent,
                locale: Locale = .autoupdatingCurrent) {
        self.now = now
        self.timeZone = timeZone
        self.locale = locale
    }

    /// This Mac's clock, zone and locale (the locale's 12- or 24-hour choice
    /// included).
    public static var system: WallClock { WallClock() }

    /// `phrase` says an RFC 3339 instant the way an alert does: "2:09 PM"
    /// today, "tomorrow 9:15 AM", "yesterday 11:40 PM", else with the day and
    /// date ("Mon, Nov 9, 9:15 AM", and the year when it is not this one). A
    /// value that does not parse is shown cleaned.
    public func phrase(_ rfc3339: String) -> String {
        guard let d = WallClock.parse(rfc3339) else {
            return Text.clean(rfc3339, max: 40)
        }
        return phrase(d)
    }

    public func phrase(_ d: Date) -> String {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = timeZone
        cal.locale = locale
        let today = now()
        let days = cal.dateComponents([.day], from: cal.startOfDay(for: today), to: cal.startOfDay(for: d)).day ?? 0

        let tf = DateFormatter()
        tf.locale = locale
        tf.timeZone = timeZone
        tf.dateStyle = .none
        tf.timeStyle = .short
        let time = WallClock.spaces(tf.string(from: d), as: "\u{00A0}")
        switch days {
        case 0:
            return time
        case 1:
            return "tomorrow " + time
        case -1:
            return "yesterday " + time
        default:
            let df = DateFormatter()
            df.locale = locale
            df.timeZone = timeZone
            let sameYear = cal.component(.year, from: d) == cal.component(.year, from: today)
            df.setLocalizedDateFormatFromTemplate(sameYear ? "EEEMMMd" : "EEEMMMdy")
            return WallClock.spaces(df.string(from: d), as: " ") + ", " + time
        }
    }

    /// `parse` reads the daemon's RFC 3339 times, with or without fractional
    /// seconds.
    static func parse(_ s: String) -> Date? {
        let plain = ISO8601DateFormatter()
        if let d = plain.date(from: s) {
            return d
        }
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: s)
    }

    /// `spaces` replaces every kind of space in a formatted date with one
    /// kind. A time keeps a no-break space, so "2:09" and "PM" never land on
    /// two lines; newer systems format en_US with U+202F, older ones with
    /// U+0020, and the tests pin one.
    static func spaces(_ s: String, as space: Character) -> String {
        String(s.map { $0.unicodeScalars.allSatisfy { $0.properties.generalCategory == .spaceSeparator } ? space : $0 })
    }
}

/// Which run a notice reports on.
public enum Flow: Equatable {
    case approve, enroll, forget

    /// The title of a run that failed.
    public var failureTitle: String {
        switch self {
        case .approve: return "Couldn't approve"
        case .enroll: return "Couldn't enroll"
        case .forget: return "Couldn't delete the key"
        }
    }
}

/// One alert at the end of a run: a title that says what happened, a body
/// of full sentences, and the lines the owner must type or match, set apart.
public struct Notice: Equatable {
    /// How long an alert that asks nothing of the owner stays up.
    public static let autoCloseSeconds: TimeInterval = 6

    public var title: String
    public var body: String
    /// `setApart` holds the lines the owner must type or match (a command
    /// with a credential id in it). They are shown apart from the body, one
    /// per line, in a monospaced field the owner can select and copy, so a
    /// wrapped body never splits an id.
    public var setApart: [String]
    public var succeeded: Bool
    /// `instructs` is whether the alert tells the owner to do something.
    public var instructs: Bool

    public init(title: String, body: String, setApart: [String] = [], succeeded: Bool, instructs: Bool = false) {
        self.title = title
        self.body = body
        self.setApart = setApart
        self.succeeded = succeeded
        self.instructs = instructs
    }

    /// `autoCloses` is the rule for the 6-second close: only a success that
    /// asks nothing of the owner closes by itself. A failure, or an alert
    /// with an instruction, stays until the owner dismisses it.
    public var autoCloses: Bool {
        succeeded && !instructs
    }

    /// `text` is the notice as plain text, for stderr: the body, then each
    /// set-apart line indented on its own line.
    public var text: String {
        ([body] + setApart.map { "  " + $0 }).joined(separator: "\n")
    }
}

/// The notices the runs end with. Every value is cleaned here or before.
public enum Notices {
    /// An approval went through. `access` is the access granted
    /// ("read-write"), `from` a read grant's asking project ("" for a lock's
    /// lease), `who` Reason.who of the request, `expires` the daemon's RFC
    /// 3339 end of the lease or grant.
    public static func approved(access: String, project: String, from: String, who: String, expires: String?,
                                clock: WallClock) -> Notice {
        let accessWord = Text.clean(access, max: 16)
        let projectName = Text.clean(project, max: 64)
        let fromName = Text.clean(from, max: 64)
        var body = (accessWord.isEmpty ? "Access" : Text.upperFirst(accessWord) + " access") + " to "
        body += projectName.isEmpty ? "the project" : projectName
        if !fromName.isEmpty {
            body += " from project " + fromName
        }
        body += " is open to " + who
        if let e = expires, !e.isEmpty {
            body += " until " + clock.phrase(e)
        }
        return Notice(title: "Approved", body: body + ".", succeeded: true)
    }

    /// The key is enrolled under `name`. `retired` is how many keys it
    /// replaced that this Mac and the daemon have both deleted. `removals`
    /// are the commands that remove the credentials of the keys it replaced,
    /// which this Mac has deleted and the daemon still lists.
    public static func enrolled(name: String, removals: [String], retired: Int = 0) -> Notice {
        var body = "This Mac's Touch ID can now approve access to locked projects. The daemon lists it as "
        body += Text.quoted(name, max: 64) + "."
        if retired == 1 {
            body += " It replaces the key this Mac enrolled before, which is deleted from this Mac and from the daemon."
        } else if retired > 1 {
            body += " It replaces the keys this Mac enrolled before, which are deleted from this Mac and from the daemon."
        }
        guard !removals.isEmpty else {
            return Notice(title: "Touch ID enrolled", body: body, succeeded: true)
        }
        if removals.count == 1 {
            body += " The key it replaces is deleted from this Mac, but the daemon still lists that key's credential."
            body += " To remove it, run this in your own terminal:"
        } else {
            body += " The keys it replaces are deleted from this Mac, but the daemon still lists their credentials."
            body += " To remove them, run these in your own terminal:"
        }
        return Notice(title: "Touch ID enrolled", body: body, setApart: removals, succeeded: true, instructs: true)
    }

    /// The key is deleted from this Mac. `credentials` are the approver ids
    /// the daemon still lists for it: with any, the alert says how to remove
    /// them, and stays open.
    public static func keyDeleted(credentials: [String]) -> Notice {
        guard !credentials.isEmpty else {
            return Notice(title: "Key deleted", body: "This Mac's Touch ID approver key is deleted.", succeeded: true)
        }
        let one = credentials.count == 1
        let body = one
            ? "This Mac's Touch ID approver key is deleted, but the daemon still lists its credential. To remove it, run this as the daemon's account:"
            : "This Mac's Touch ID approver keys are deleted, but the daemon still lists their credentials. To remove them, run these as the daemon's account:"
        return Notice(title: "Key deleted", body: body, setApart: credentials.map { "seamlessd approvers remove " + $0 },
                      succeeded: true, instructs: true)
    }

    /// Forget found no key.
    public static func noKey() -> Notice {
        Notice(title: "No key to delete", body: "No Touch ID approver key is stored on this Mac, so there is nothing to delete.",
               succeeded: true)
    }

    /// A run failed with `message`, a sentence or a clause.
    public static func failed(_ flow: Flow, _ message: String) -> Notice {
        Notice(title: flow.failureTitle, body: Text.sentence(message), succeeded: false)
    }

    /// The helper was started with a command line it does not take.
    public static func badArguments(_ problem: String) -> Notice {
        let lines = Arguments.usage.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }
        return Notice(title: "Couldn't start",
                      body: Text.sentence("Seamless Approve was started with arguments it does not take: " + Text.clean(problem, max: 300))
                          + " It takes these:",
                      setApart: lines, succeeded: false)
    }
}

/// A dialog that asks the owner something.
public struct Prompt: Equatable {
    public var title: String
    public var body: String
    /// `setApart` is shown monospaced and selectable, apart from the body.
    public var setApart: [String]
    /// The code field's placeholder; nil for a dialog without a field.
    public var placeholder: String?
    public var accept: String
    public var cancel: String
}

public enum Prompts {
    /// The placeholder of the code field: the shape the daemon prints a code
    /// in (store.MintEnrollCode).
    public static let codeShape = "XXXX-XXXX-XXXX-XXXX"

    /// `enrollCode` asks for the one-time code that `command` mints.
    /// `problem` says what was wrong with the last code typed.
    public static func enrollCode(command: String, problem: String?) -> Prompt {
        var body = ""
        if let problem = problem, !problem.isEmpty {
            body += Text.sentence(problem) + "\n\n"
        }
        body += "Run this command in your own terminal. It asks for your password, then prints a one-time enrollment code. "
        body += "Type the code below, with or without its dashes. A code works once, for 10 minutes."
        return Prompt(title: "Enroll this Mac's Touch ID with Seamless", body: body, setApart: [command],
                      placeholder: codeShape, accept: "Enroll", cancel: "Cancel")
    }

    /// `confirmForget` asks before this Mac's key is deleted. `keptByDaemon`
    /// is whether the daemon lists the key's credential.
    public static func confirmForget(keptByDaemon: Bool) -> Prompt {
        var body = "Seamless Approve will no longer be able to approve access from this Mac until you enroll again with seam touchid enroll."
        if keptByDaemon {
            body += " The daemon keeps the key's credential until its account removes it. The next window shows the command."
        }
        return Prompt(title: "Delete this Mac's Touch ID approver key?", body: body, setApart: [], placeholder: nil,
                      accept: "Delete Key", cancel: "Cancel")
    }
}

/// The problems the code field reports, as sentences.
public enum CodeProblem {
    public static let notASCII = "An enrollment code holds only letters, digits and dashes."
    public static let notACode = "That is not an enrollment code. A code has 16 letters and digits, printed as XXXX-XXXX-XXXX-XXXX."
}

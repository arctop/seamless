import AppKit
import SeamApproveCore
import SystemConfiguration

// Seamless Approve: one approval, one enrollment or one forget per launch.
//
//   approve --socket <path> --request <id> --for <duration>
//   enroll  --socket <path> [--name <label>]
//   forget
//
// Exit status: 0 done, 1 failed, 2 cancelled by the owner. Started through
// LaunchServices (open -n -a ... --args ...), the app has no terminal, so it
// also shows the outcome in its own alert; run from a terminal, stderr is
// enough and only the dialogs that ask something appear.

final class AppDelegate: NSObject, NSApplicationDelegate {
    let argv: [String]
    let interactive: Bool
    let ui = AppUI()

    init(argv: [String], interactive: Bool) {
        self.argv = argv
        self.interactive = interactive
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        Thread.detachNewThread { [argv, ui] in
            let outcome = Runner.run(argv: argv, ui: ui)
            DispatchQueue.main.async {
                self.finish(outcome)
            }
        }
    }

    func finish(_ outcome: Outcome) {
        FileHandle.standardError.write(Data(("SeamApprove: " + outcome.message + "\n").utf8))
        // A cancel shows nothing: the owner just chose it.
        if !interactive, let notice = outcome.notice {
            ui.show(notice)
        }
        exit(outcome.exitCode)
    }
}

enum Runner {
    static var version: String {
        Bundle.main.object(forInfoDictionaryKey: "SeamlessVersion") as? String ?? "dev"
    }

    static func run(argv: [String], ui: AppUI) -> Outcome {
        let command: Command
        do {
            command = try Arguments.parse(argv)
        } catch {
            return .failed(Notices.badArguments("\(error)"))
        }
        switch command {
        case let .approve(socket, request, duration):
            return flows(socket: socket, ui: ui).approve(id: request, duration: duration)
        case let .enroll(socket, name):
            return flows(socket: socket, ui: ui).enroll(name: name)
        case .forget:
            return flows(socket: nil, ui: ui).forget()
        }
    }

    static func flows(socket: String?, ui: AppUI) -> Flows {
        let peer = PeerGuard(ownUID: getuid())
        let transport: Transport = socket.map {
            UnixSocketTransport(path: $0, userAgent: "SeamlessApprove/" + version, admit: peer.admit)
        } ?? NoTransport()
        return Flows(transport: transport, keys: SecureEnclaveKeyStore(), ui: ui, lookupAccount: uidOfAccount,
                     defaultName: { SCDynamicStoreCopyComputerName(nil, nil) as String? ?? "Touch ID" })
    }
}

/// `forget` talks to no daemon.
struct NoTransport: Transport {
    func exchange(_ request: HTTPRequest) throws -> Exchange {
        throw ApproverError.transport("No daemon socket was given.")
    }
}

signal(SIGPIPE, SIG_IGN)
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
app.mainMenu = App.editMenu()
let delegate = AppDelegate(argv: Array(CommandLine.arguments.dropFirst()), interactive: isatty(STDERR_FILENO) != 0)
app.delegate = delegate
app.run()

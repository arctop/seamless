import AppKit
import SeamApproveCore

/// The helper's own dialogs. The flows run on a worker thread; every dialog
/// runs modally on the main thread.
final class AppUI: ApproverUI {
    func enrollCode(command: String, problem: String?) -> String? {
        onMain {
            let alert = NSAlert()
            alert.messageText = "Enroll this Mac's Touch ID with Seamless"
            var info = ""
            if let problem = problem {
                info += problem + "\n\n"
            }
            info += "Type the one-time enrollment code that this command prints in your own terminal:\n\n"
            info += command + "\n\n"
            info += "A code works once and lasts 10 minutes. It is typed here, never passed on a command line."
            alert.informativeText = info
            let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 300, height: 24))
            field.placeholderString = "XXXX-XXXX-XXXX-XXXX"
            field.font = NSFont.monospacedSystemFont(ofSize: 14, weight: .regular)
            alert.accessoryView = field
            alert.addButton(withTitle: "Enroll")
            alert.addButton(withTitle: "Cancel")
            alert.window.initialFirstResponder = field
            App.bringForward()
            return alert.runModal() == .alertFirstButtonReturn ? field.stringValue : nil
        }
    }

    func confirmForget(credentials: [String]) -> Bool {
        onMain {
            let alert = NSAlert()
            alert.alertStyle = .warning
            alert.messageText = "Delete this Mac's Touch ID approver key?"
            var info = "Seamless Approve will no longer be able to approve access from this Mac until you enroll again with seam touchid enroll."
            if !credentials.isEmpty {
                info += "\n\nThe daemon keeps the credential (\(credentials.joined(separator: ", "))) until its account removes it with seamlessd approvers remove."
            }
            alert.informativeText = info
            alert.addButton(withTitle: "Delete Key")
            alert.addButton(withTitle: "Cancel")
            App.bringForward()
            return alert.runModal() == .alertFirstButtonReturn
        }
    }

    /// `showOutcome` tells the owner how the run ended, since a helper that
    /// LaunchServices started has no terminal. A success closes by itself.
    func showOutcome(_ outcome: Outcome) {
        onMain {
            let alert = NSAlert()
            switch outcome {
            case .done:
                alert.alertStyle = .informational
                alert.messageText = "Seamless Approve"
            case .failed:
                alert.alertStyle = .warning
                alert.messageText = "Seamless Approve did not finish"
            case .cancelled:
                alert.alertStyle = .informational
                alert.messageText = "Seamless Approve: cancelled"
            }
            alert.informativeText = outcome.message
            alert.addButton(withTitle: "OK")
            var timer: Timer?
            if case .done = outcome {
                let t = Timer(timeInterval: 6, repeats: false) { _ in
                    NSApp.abortModal()
                }
                RunLoop.main.add(t, forMode: .modalPanel)
                timer = t
            }
            App.bringForward()
            _ = alert.runModal()
            timer?.invalidate()
        }
    }
}

func onMain<T>(_ body: () -> T) -> T {
    if Thread.isMainThread {
        return body()
    }
    return DispatchQueue.main.sync(execute: body)
}

enum App {
    static func bringForward() {
        if #available(macOS 14.0, *) {
            NSApp.activate()
        } else {
            NSApp.activate(ignoringOtherApps: true)
        }
    }

    /// An Edit menu, never shown (the app has no menu bar: LSUIElement), so
    /// that Command-V pastes into the enrollment code's field.
    static func editMenu() -> NSMenu {
        let main = NSMenu()
        let editItem = NSMenuItem()
        main.addItem(editItem)
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        editItem.submenu = edit
        return main
    }
}

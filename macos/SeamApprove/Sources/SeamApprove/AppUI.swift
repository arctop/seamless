import AppKit
import SeamApproveCore

/// The helper's own dialogs. The flows run on a worker thread; every dialog
/// runs modally on the main thread. The words come from SeamApproveCore
/// (Prompts, Notices), where the tests pin them; this file only lays them out.
final class AppUI: ApproverUI {
    func enrollCode(command: String, problem: String?) -> String? {
        onMain {
            let p = Prompts.enrollCode(command: command, problem: problem)
            let alert = NSAlert()
            alert.messageText = p.title
            alert.informativeText = p.body
            let field = NSTextField(frame: NSRect(x: 0, y: 0, width: App.accessoryWidth, height: 24))
            field.placeholderString = p.placeholder
            field.font = NSFont.monospacedSystemFont(ofSize: 14, weight: .regular)
            alert.accessoryView = App.stack([App.setApartField(p.setApart), field])
            alert.addButton(withTitle: p.accept)
            alert.addButton(withTitle: p.cancel)
            alert.window.initialFirstResponder = field
            App.bringForward()
            return alert.runModal() == .alertFirstButtonReturn ? field.stringValue : nil
        }
    }

    func confirmForget(credentials: [String]) -> Bool {
        onMain {
            let p = Prompts.confirmForget(keptByDaemon: !credentials.isEmpty)
            let alert = NSAlert()
            alert.alertStyle = .warning
            alert.messageText = p.title
            alert.informativeText = p.body
            alert.addButton(withTitle: p.accept)
            alert.addButton(withTitle: p.cancel)
            App.bringForward()
            return alert.runModal() == .alertFirstButtonReturn
        }
    }

    /// `show` tells the owner how the run ended, since a helper that
    /// LaunchServices started has no terminal. It closes by itself after
    /// Notice.autoCloseSeconds only when it asks nothing of the owner
    /// (Notice.autoCloses).
    func show(_ notice: Notice) {
        onMain {
            let alert = NSAlert()
            alert.alertStyle = notice.succeeded ? .informational : .warning
            alert.messageText = notice.title
            alert.informativeText = notice.body
            if !notice.setApart.isEmpty {
                alert.accessoryView = App.setApartField(notice.setApart)
            }
            alert.addButton(withTitle: "OK")
            var timer: Timer?
            if notice.autoCloses {
                let t = Timer(timeInterval: Notice.autoCloseSeconds, repeats: false) { _ in
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
    /// The width of an alert's accessory view. At 12 points a monospaced
    /// character is about 7.2 points wide, so a line holds some 47 of them:
    /// a 26-symbol credential id always fits on one line, and word wrapping
    /// breaks a command only at its spaces.
    static let accessoryWidth: CGFloat = 340

    static func bringForward() {
        if #available(macOS 14.0, *) {
            NSApp.activate()
        } else {
            NSApp.activate(ignoringOtherApps: true)
        }
    }

    /// `setApartField` shows lines the owner must type or match: monospaced,
    /// selectable (to copy), not editable, one paragraph per line, wrapped at
    /// spaces only where a line is longer than the field.
    static func setApartField(_ lines: [String]) -> NSTextField {
        let field = NSTextField(wrappingLabelWithString: lines.joined(separator: "\n"))
        field.font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        field.isEditable = false
        field.isSelectable = true
        field.isBezeled = true
        field.bezelStyle = .squareBezel
        field.drawsBackground = true
        field.backgroundColor = .textBackgroundColor
        field.textColor = .labelColor
        field.lineBreakMode = .byWordWrapping
        field.preferredMaxLayoutWidth = accessoryWidth
        let bounds = NSRect(x: 0, y: 0, width: accessoryWidth, height: 10_000)
        let height = ceil(field.cell?.cellSize(forBounds: bounds).height ?? 24)
        field.frame = NSRect(x: 0, y: 0, width: accessoryWidth, height: height)
        return field
    }

    /// `stack` lays views out top to bottom in one accessory view.
    static func stack(_ views: [NSView], spacing: CGFloat = 8) -> NSView {
        let height = views.reduce(0) { $0 + $1.frame.height } + spacing * CGFloat(max(views.count - 1, 0))
        let box = NSView(frame: NSRect(x: 0, y: 0, width: accessoryWidth, height: height))
        var top = height
        for v in views {
            top -= v.frame.height
            v.setFrameOrigin(NSPoint(x: 0, y: top))
            box.addSubview(v)
            top -= spacing
        }
        return box
    }

    /// An Edit menu, never shown (the app has no menu bar: LSUIElement), so
    /// that Command-V pastes into the enrollment code's field and Command-C
    /// copies from a set-apart command.
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

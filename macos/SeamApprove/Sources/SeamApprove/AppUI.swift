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
            let command = SetApartView(lines: p.setApart, copyable: p.copyable, width: App.accessoryWidth)
            alert.accessoryView = App.stack([command, field])
            alert.addButton(withTitle: p.accept)
            alert.addButton(withTitle: p.cancel)
            alert.window.initialFirstResponder = field
            // The owner mints the code in another terminal while this dialog
            // waits. The app has no Dock icon and no Command-Tab entry
            // (LSUIElement), so the dialog must not hide or sink behind that
            // terminal when the app is no longer frontmost.
            alert.window.hidesOnDeactivate = false
            if alert.window.level.rawValue < NSWindow.Level.floating.rawValue {
                alert.window.level = .floating
            }
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
    /// LaunchServices started has no terminal. A failure is marked as one:
    /// the critical style's caution badge on the app icon, and the title in
    /// red. It closes by itself after Notice.autoCloseSeconds only when it
    /// changed nothing and asks nothing of the owner (Notice.autoCloses).
    func show(_ notice: Notice) {
        onMain {
            let alert = NSAlert()
            alert.alertStyle = notice.succeeded ? .informational : .critical
            alert.messageText = notice.title
            alert.informativeText = notice.alertBody
            if !notice.setApart.isEmpty {
                alert.accessoryView = SetApartView(lines: notice.setApart, copyable: notice.copyable, width: App.accessoryWidth)
            }
            alert.addButton(withTitle: "OK")
            if !notice.succeeded {
                // Once now and again once the modal session runs: runModal
                // lays the alert out afresh and resets the title's style.
                App.tintTitle(of: alert, App.failureRed)
                RunLoop.main.perform(inModes: [.modalPanel]) {
                    App.tintTitle(of: alert, App.failureRed, layout: false)
                }
            }
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
    /// character is about 7.2 points wide, so a line in the set-apart box
    /// holds some 45 of them: a 26-symbol credential id always fits on one
    /// line. Word wrapping breaks a command at its spaces, and inside a path
    /// longer than a line, after a slash.
    static let accessoryWidth: CGFloat = 340

    /// The red of a failure alert's title. An alert draws its title lighter
    /// than the color it is given, so in the light appearance the system red
    /// came out at 1.4 to 2:1 against the alert's glass. This darker red, as
    /// drawn, measures 3:1 on glass over a dark window and better than 4.5:1
    /// on lighter glass. The dark appearance keeps the system red: reaching
    /// 3:1 there takes a pale salmon that no longer reads as red.
    static let failureRed = NSColor(name: nil) { appearance in
        if appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua {
            return .systemRed
        }
        return NSColor(srgbRed: 160.0 / 255.0, green: 0, blue: 0, alpha: 1)
    }

    static func bringForward() {
        if #available(macOS 14.0, *) {
            NSApp.activate()
        } else {
            NSApp.activate(ignoringOtherApps: true)
        }
    }

    /// `tintTitle` colors an alert's title. NSAlert has no setting for it,
    /// so this finds the label that shows the title (laying the alert out
    /// first when asked) and colors its text, as an attribute of the text
    /// as well as the label's color, since the alert styles the title with
    /// attributes of its own. On a system that builds the alert some other
    /// way the title keeps its color, and the critical style's badge still
    /// marks the failure.
    static func tintTitle(of alert: NSAlert, _ color: NSColor, layout: Bool = true) {
        if layout {
            alert.layout()
        }
        guard let root = alert.window.contentView else {
            return
        }
        var queue: [NSView] = [root]
        while !queue.isEmpty {
            let v = queue.removeFirst()
            if let label = v as? NSTextField, !label.isEditable, label.stringValue == alert.messageText {
                let text = NSMutableAttributedString(attributedString: label.attributedStringValue)
                text.addAttribute(.foregroundColor, value: color, range: NSRange(location: 0, length: text.length))
                label.textColor = color
                label.attributedStringValue = text
                return
            }
            queue.append(contentsOf: v.subviews)
        }
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
    /// copies a selection from set-apart lines that a click does not copy.
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

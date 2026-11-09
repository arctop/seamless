import AppKit
import SeamApproveCore

/// `SetApartView` shows set-apart lines in a monospaced box under an alert's
/// body: the command the owner runs, or lines to match. The box is as tall as
/// the text the box itself lays out, so no wrapped line falls below its edge
/// (a bezeled NSTextField's measured cell size counted one wrap fewer than it
/// drew, which hid the last line of the enrollment command). A copyable box
/// copies every line on a click, with the hint underneath saying so first and
/// confirming it after.
final class SetApartView: NSView {
    static let font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
    static let hintFont = NSFont.systemFont(ofSize: NSFont.smallSystemFontSize)
    /// Room around the text inside the box.
    static let inset = NSSize(width: 4, height: 5)
    /// Between the box and the hint under it.
    static let hintSpacing: CGFloat = 4

    private let lines: [String]
    private let hint: NSTextField?

    init(lines: [String], copyable: Bool, width: CGFloat) {
        self.lines = lines

        // TextKit 1, so the layout manager that measures is the one that draws.
        let text = CopyTextView(usingTextLayoutManager: false)
        let shown = lines.joined(separator: "\n")
        // A copyable box is never selected (a click copies the lines as
        // given), so what it shows may differ invisibly: a word joiner after
        // each hyphen keeps "enroll-code" from wrapping into what reads as
        // two words. A selectable box shows the lines exactly.
        text.string = copyable ? shown.replacingOccurrences(of: "-", with: "-\u{2060}") : shown
        text.font = Self.font
        text.textColor = .labelColor
        text.isEditable = false
        text.isSelectable = !copyable
        text.drawsBackground = false
        text.isRichText = false
        text.textContainerInset = Self.inset
        text.isVerticallyResizable = false
        text.isHorizontallyResizable = false
        text.textContainer?.widthTracksTextView = true
        text.textContainer?.lineBreakMode = .byWordWrapping

        let box = NSBox(frame: NSRect(x: 0, y: 0, width: width, height: 100))
        box.boxType = .custom
        box.titlePosition = .noTitle
        box.borderWidth = 1
        box.cornerRadius = 5
        box.borderColor = .separatorColor
        box.fillColor = .textBackgroundColor
        box.contentViewMargins = .zero
        box.contentView = text
        // The box gives the text its content width; the height follows from
        // laying the text out at that width, plus the box's border.
        let border = box.frame.height - text.frame.height
        var textHeight: CGFloat = 0
        if let layout = text.layoutManager, let container = text.textContainer {
            layout.ensureLayout(for: container)
            textHeight = ceil(layout.usedRect(for: container).height + 2 * Self.inset.height)
        }
        box.setFrameSize(NSSize(width: width, height: textHeight + border))

        var height = box.frame.height
        if copyable {
            let label = NSTextField(wrappingLabelWithString: CopyHint.before(lines: lines.count))
            label.font = Self.hintFont
            label.textColor = .secondaryLabelColor
            label.preferredMaxLayoutWidth = width
            let size = label.cell?.cellSize(forBounds: NSRect(x: 0, y: 0, width: width, height: 1_000)) ?? .zero
            label.frame = NSRect(x: 0, y: 0, width: width, height: ceil(size.height))
            hint = label
            height += Self.hintSpacing + label.frame.height
        } else {
            hint = nil
        }

        super.init(frame: NSRect(x: 0, y: 0, width: width, height: height))
        box.setFrameOrigin(NSPoint(x: 0, y: height - box.frame.height))
        addSubview(box)
        if let hint = hint {
            addSubview(hint)
            text.onClick = { [weak self] in self?.copyAll() }
            text.setAccessibilityHelp(hint.stringValue)
        }
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("SetApartView is built in code")
    }

    /// `copyAll` puts every line on the clipboard and says so.
    func copyAll() {
        let board = NSPasteboard.general
        board.clearContents()
        board.setString(CopyHint.clipboard(lines), forType: .string)
        showCopied()
    }

    /// `showCopied` turns the hint into its confirmation.
    func showCopied() {
        hint?.stringValue = CopyHint.after
    }
}

/// `CopyTextView` is the box's text: with `onClick` set, a click anywhere on
/// it runs that (the copy) instead of starting a selection, and the pointer
/// over it is a pointing hand.
final class CopyTextView: NSTextView {
    var onClick: (() -> Void)?

    override func mouseDown(with event: NSEvent) {
        guard let onClick = onClick else {
            super.mouseDown(with: event)
            return
        }
        onClick()
    }

    override func resetCursorRects() {
        guard onClick != nil else {
            super.resetCursorRects()
            return
        }
        addCursorRect(bounds, cursor: .pointingHand)
    }

    override func cursorUpdate(with event: NSEvent) {
        guard onClick != nil else {
            super.cursorUpdate(with: event)
            return
        }
        NSCursor.pointingHand.set()
    }
}

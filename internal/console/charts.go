package console

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// anyCoverage reports whether any bucket in the window had at least one session.
func anyCoverage(buckets []store.CoverageBucket) bool {
	for _, b := range buckets {
		if b.Total > 0 {
			return true
		}
	}
	return false
}

// This file holds the console's self-contained visual primitives: inline Lucide
// icons and hand-rolled SVG/flex charts. They render server-side to
// template.HTML (no charting library, no client JS), matching the "Seamless
// Console Design Brief" reference UI kit. Every value fed in is
// server-controlled (counts, enum kinds); free-form strings are escaped.

// ---------------------------------------------------------------------------
// Icons -- a small inline Lucide set (1.75px stroke, rounded, geometric).
// ---------------------------------------------------------------------------

// lucidePaths maps an icon name to the inner markup of a 24x24 Lucide glyph.
var lucidePaths = map[string]string{
	"layout-dashboard":      `<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>`,
	"terminal":              `<polyline points="4 17 10 11 4 5"/><line x1="12" x2="20" y1="19" y2="19"/>`,
	"activity":              `<polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>`,
	"database":              `<ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5V19A9 3 0 0 0 21 19V5"/><path d="M3 12A9 3 0 0 0 21 12"/>`,
	"arrow-down-to-line":    `<path d="M12 17V3"/><path d="m6 11 6 6 6-6"/><path d="M19 21H5"/>`,
	"list-checks":           `<path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/>`,
	"file-text":             `<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/>`,
	"sprout":                `<path d="M7 20h10"/><path d="M10 20c5.5-2.5.8-6.4 3-10"/><path d="M9.5 9.4c1.1.8 1.8 2.2 2.3 3.7-2 .4-3.5.4-4.8-.3-1.2-.6-2.3-1.9-3-4.2 2.8-.5 4.4 0 5.5.8z"/><path d="M14.1 6a7 7 0 0 0-1.1 4c1.9-.1 3.3-.6 4.3-1.4 1-1 1.6-2.3 1.7-4.6-2.7.1-4 1-4.9 2z"/>`,
	"settings":              `<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>`,
	"sun":                   `<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>`,
	"moon":                  `<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>`,
	"log-out":               `<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/>`,
	"map":                   `<path d="M14.106 5.553a2 2 0 0 0 1.788 0l3.659-1.83A1 1 0 0 1 21 4.619v12.764a1 1 0 0 1-.553.894l-4.553 2.277a2 2 0 0 1-1.788 0l-4.212-2.106a2 2 0 0 0-1.788 0l-3.659 1.83A1 1 0 0 1 3 19.381V6.618a1 1 0 0 1 .553-.894l4.553-2.277a2 2 0 0 1 1.788 0z"/><path d="M15 5.764v15"/><path d="M9 3.236v15"/>`,
	"triangle-alert":        `<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"/><path d="M12 9v4"/><path d="M12 17h.01"/>`,
	"copy":                  `<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>`,
	"check":                 `<path d="M20 6 9 17l-5-5"/>`,
	"star":                  `<path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.123 2.123 0 0 0 1.595 1.16l5.166.756a.53.53 0 0 1 .294.904l-3.736 3.638a2.123 2.123 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.122 2.122 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.122 2.122 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.755a2.122 2.122 0 0 0 1.597-1.16z"/>`,
	"search":                `<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>`,
	"table-2":               `<path d="M9 3H5a2 2 0 0 0-2 2v4m6-6h10a2 2 0 0 1 2 2v4M9 3v18m0 0h10a2 2 0 0 0 2-2V9M9 21H5a2 2 0 0 1-2-2V9m0 0h18"/>`,
	"server":                `<rect width="20" height="8" x="2" y="2" rx="2" ry="2"/><rect width="20" height="8" x="2" y="14" rx="2" ry="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>`,
	"folder-tree":           `<path d="M20 10a1 1 0 0 0 1-1V6a1 1 0 0 0-1-1h-2.5a1 1 0 0 1-.8-.4l-.9-1.2A1 1 0 0 0 15 3h-2a1 1 0 0 0-1 1v5a1 1 0 0 0 1 1Z"/><path d="M20 21a1 1 0 0 0 1-1v-3a1 1 0 0 0-1-1h-2.9a1 1 0 0 1-.88-.55l-.42-.85a1 1 0 0 0-.92-.6H13a1 1 0 0 0-1 1v5a1 1 0 0 0 1 1Z"/><path d="M3 5a2 2 0 0 0 2 2h3"/><path d="M3 3v13a2 2 0 0 0 2 2h3"/>`,
	"box":                   `<path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z"/><path d="m3.3 7 8.7 5 8.7-5"/><path d="M12 22V12"/>`,
	"archive":               `<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>`,
	"git-fork":              `<circle cx="12" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><circle cx="18" cy="6" r="3"/><path d="M18 9v1a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V9"/><path d="M12 12v3"/>`,
	"link":                  `<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>`,
	"git-branch":            `<line x1="6" x2="6" y1="3" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>`,
	"git-merge":             `<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M6 21V9a9 9 0 0 0 9 9"/>`,
	"git-commit-horizontal": `<circle cx="12" cy="12" r="3"/><line x1="3" x2="9" y1="12" y2="12"/><line x1="15" x2="21" y1="12" y2="12"/>`,
	"share-2":               `<circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><line x1="8.59" x2="15.42" y1="13.51" y2="17.49"/><line x1="15.41" x2="8.59" y1="6.51" y2="10.49"/>`,
	"split":                 `<path d="M16 3h5v5"/><path d="M8 3H3v5"/><path d="M12 22v-8.3a4 4 0 0 0-1.172-2.872L3 3"/><path d="m15 9 6-6"/>`,
	"gauge":                 `<path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/>`,
	"trending-up":           `<polyline points="22 7 13.5 15.5 8.5 10.5 2 17"/><polyline points="16 7 22 7 22 13"/>`,
	"brain":                 `<path d="M12 5a3 3 0 1 0-5.997.125 4 4 0 0 0-2.526 5.77 4 4 0 0 0 .556 6.588A4 4 0 1 0 12 18Z"/><path d="M12 5a3 3 0 1 1 5.997.125 4 4 0 0 1 2.526 5.77 4 4 0 0 1-.556 6.588A4 4 0 1 1 12 18Z"/><path d="M15 13a4.5 4.5 0 0 1-3-4 4.5 4.5 0 0 1-3 4"/>`,
	"circle":                `<circle cx="12" cy="12" r="10"/>`,
	"loader":                `<line x1="12" x2="12" y1="2" y2="6"/><line x1="12" x2="12" y1="18" y2="22"/><line x1="4.93" x2="7.76" y1="4.93" y2="7.76"/><line x1="16.24" x2="19.07" y1="16.24" y2="19.07"/><line x1="2" x2="6" y1="12" y2="12"/><line x1="18" x2="22" y1="12" y2="12"/><line x1="4.93" x2="7.76" y1="19.07" y2="16.24"/><line x1="16.24" x2="19.07" y1="7.76" y2="4.93"/>`,
	"lock":                  `<rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>`,
	"key-round":             `<path d="M2.586 17.414A2 2 0 0 0 2 18.828V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.172a2 2 0 0 0 1.414-.586l.814-.814a6.5 6.5 0 1 0-4-4z"/><circle cx="16.5" cy="7.5" r=".5" fill="currentColor"/>`,
	"timer":                 `<line x1="10" x2="14" y1="2" y2="2"/><line x1="12" x2="15" y1="14" y2="11"/><circle cx="12" cy="14" r="8"/>`,
	"arrow-right":           `<path d="M5 12h14"/><path d="m12 5 7 7-7 7"/>`,
	"chevron-left":          `<path d="m15 18-6-6 6-6"/>`,
	"chevron-right":         `<path d="m9 18 6-6-6-6"/>`,
	"arrow-up-down":         `<path d="m21 16-4 4-4-4"/><path d="M17 20V4"/><path d="m3 8 4-4 4 4"/><path d="M7 4v16"/>`,
	"undo-2":                `<path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5 5.5 5.5 0 0 1-5.5 5.5H11"/>`,
	"folder-open":           `<path d="m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2"/>`,
	"flask-conical":         `<path d="M10 2v7.527a2 2 0 0 1-.211.896L4.72 20.55a1 1 0 0 0 .9 1.45h12.76a1 1 0 0 0 .9-1.45l-5.069-10.127A2 2 0 0 1 14 9.527V2"/><path d="M8.5 2h7"/><path d="M7 16h10"/>`,
	"test-tube":             `<path d="M14.5 2v17.5c0 1.4-1.1 2.5-2.5 2.5c-1.4 0-2.5-1.1-2.5-2.5V2"/><path d="M8.5 2h7"/><path d="M14.5 16h-5"/>`,
	"eye":                   `<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>`,
	"eye-off":               `<path d="M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.747 10.747 0 0 1-1.444 2.49"/><path d="M14.084 14.158a3 3 0 0 1-4.242-4.242"/><path d="M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143"/><path d="m2 2 20 20"/>`,
	"toggle-right":          `<rect width="20" height="12" x="2" y="6" rx="6" ry="6"/><circle cx="16" cy="12" r="2"/>`,
	"flag":                  `<path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/><line x1="4" x2="4" y1="22" y2="15"/>`,
	"calendar-days":         `<path d="M8 2v4"/><path d="M16 2v4"/><rect width="18" height="18" x="3" y="4" rx="2"/><path d="M3 10h18"/><path d="M8 14h.01"/><path d="M12 14h.01"/><path d="M16 14h.01"/><path d="M8 18h.01"/><path d="M12 18h.01"/><path d="M16 18h.01"/>`,
	"leaf":                  `<path d="M11 20A7 7 0 0 1 9.8 6.1C15.5 5 17 4.48 19 2c1 2 2 4.18 2 8 0 5.5-4.78 10-10 10Z"/><path d="M2 21c0-3 1.85-5.36 5.08-6C9.5 14.52 12 13 13 12"/>`,
	"tree-pine":             `<path d="m17 14 3 3.3a1 1 0 0 1-.7 1.7H4.7a1 1 0 0 1-.7-1.7L7 14h-.3a1 1 0 0 1-.7-1.7L9 9h-.2A1 1 0 0 1 8 7.3L12 3l4 4.3a1 1 0 0 1-.8 1.7H15l3 3.3a1 1 0 0 1-.7 1.7H17Z"/><path d="M12 22v-3"/>`,
	"power":                 `<path d="M12 2v10"/><path d="M18.4 6.6a9 9 0 1 1-12.77.04"/>`,
	"radar":                 `<path d="M19.07 4.93A10 10 0 0 0 6.99 3.34"/><path d="M4 6h.01"/><path d="M2.29 9.62A10 10 0 1 0 21.31 8.35"/><path d="M16.24 7.76A6 6 0 1 0 8.23 16.67"/><path d="M12 18h.01"/><path d="M17.99 11.66A6 6 0 0 1 15.77 16.67"/><circle cx="12" cy="12" r="2"/><path d="m13.41 10.59 5.66-5.66"/>`,
	"flame":                 `<path d="M8.5 14.5A2.5 2.5 0 0 0 11 12c0-1.38-.5-2-1-3-1.072-2.143-.224-4.054 2-6 .5 2.5 2 4.9 4 6.5 2 1.6 3 3.5 3 5.5a7 7 0 1 1-14 0c0-1.153.433-2.294 1-3a2.5 2.5 0 0 0 2.5 2.5z"/>`,
	"trophy":                `<path d="M6 9H4.5a2.5 2.5 0 0 1 0-5H6"/><path d="M18 9h1.5a2.5 2.5 0 0 0 0-5H18"/><path d="M4 22h16"/><path d="M10 14.66V17c0 .55-.47.98-.97 1.21C7.85 18.75 7 20.24 7 22"/><path d="M14 14.66V17c0 .55.47.98.97 1.21C16.15 18.75 17 20.24 17 22"/><path d="M18 2H6v7a6 6 0 0 0 12 0V2Z"/>`,
	"zap":                   `<path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/>`,
	"award":                 `<path d="m15.477 12.89 1.515 8.526a.5.5 0 0 1-.81.47l-3.58-2.687a1 1 0 0 0-1.197 0l-3.586 2.686a.5.5 0 0 1-.81-.469l1.514-8.526"/><circle cx="12" cy="8" r="6"/>`,
	// Shell chrome: sidebar collapse, phone drawer, the shortcut sheet, the
	// palette's jump/recent/action groups, and the page-purpose disclosure.
	"panel-left":       `<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/>`,
	"menu":             `<line x1="4" x2="20" y1="12" y2="12"/><line x1="4" x2="20" y1="6" y2="6"/><line x1="4" x2="20" y1="18" y2="18"/>`,
	"x":                `<path d="M18 6 6 18"/><path d="m6 6 12 12"/>`,
	"keyboard":         `<path d="M10 8h.01"/><path d="M12 12h.01"/><path d="M14 8h.01"/><path d="M16 12h.01"/><path d="M18 8h.01"/><path d="M6 8h.01"/><path d="M7 16h10"/><path d="M8 12h.01"/><rect width="20" height="16" x="2" y="4" rx="2"/>`,
	"compass":          `<path d="m16.24 7.76-1.804 5.411a2 2 0 0 1-1.265 1.265L7.76 16.24l1.804-5.411a2 2 0 0 1 1.265-1.265z"/><circle cx="12" cy="12" r="10"/>`,
	"clock":            `<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>`,
	"history":          `<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/>`,
	"info":             `<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>`,
	"chevron-down":     `<path d="m6 9 6 6 6-6"/>`,
	"arrow-up-right":   `<path d="M7 7h10v10"/><path d="M7 17 17 7"/>`,
	"external-link":    `<path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>`,
	"pencil":           `<path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z"/><path d="m15 5 4 4"/>`,
	"sparkles":         `<path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z"/><path d="M20 3v4"/><path d="M22 5h-4"/><path d="M4 17v2"/><path d="M5 18H3"/>`,
	"corner-down-left": `<polyline points="9 10 4 15 9 20"/><path d="M20 4v7a4 4 0 0 1-4 4H4"/>`,
	"refresh-cw":       `<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>`,
	"sliders":          `<line x1="21" x2="14" y1="4" y2="4"/><line x1="10" x2="3" y1="4" y2="4"/><line x1="21" x2="12" y1="12" y2="12"/><line x1="8" x2="3" y1="12" y2="12"/><line x1="21" x2="16" y1="20" y2="20"/><line x1="12" x2="3" y1="20" y2="20"/><line x1="14" x2="14" y1="2" y2="6"/><line x1="8" x2="8" y1="10" y2="14"/><line x1="16" x2="16" y1="18" y2="22"/>`,
	"inbox":            `<polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>`,
}

// icon renders a named Lucide glyph as an inline SVG that inherits currentColor.
// Unknown names render nothing so a typo can't break a page.
func icon(name string) template.HTML {
	p, ok := lucidePaths[name]
	if !ok {
		return ""
	}
	return template.HTML(`<svg class="ico ico-` + name + `" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + p + `</svg>`)
}

// ---------------------------------------------------------------------------
// Color mappings
// ---------------------------------------------------------------------------

// kindColorVar returns the CSS color for a memory kind (the same per-kind hues
// the legend/chart dots use). Unknown kinds fall back to the muted tone.
func kindColorVar(kind string) string {
	switch kind {
	case "gotcha", "constraint", "convention", "reference", "decision", "runbook", "protocol", "stage", "refuted":
		return "var(--k-" + kind + ")"
	default:
		return "var(--muted)"
	}
}

// ---------------------------------------------------------------------------
// Kind legend -- memories by kind
// ---------------------------------------------------------------------------

// kindLegend renders the memories-by-kind breakdown as labeled bars, largest
// kind first, widths normalized to the largest.
func kindLegend(items []kindCount) template.HTML {
	if len(items) == 0 {
		return ""
	}
	maxV := 1
	for _, it := range items {
		if it.N > maxV {
			maxV = it.N
		}
	}
	sorted := make([]kindCount, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].N > sorted[j].N })
	var b strings.Builder
	b.WriteString(`<div class="legend">`)
	for _, it := range sorted {
		c := kindColorVar(it.Kind)
		fmt.Fprintf(&b, `<div class="legend-row"><span class="kdot" style="background:%s"></span><span class="legend-label">%s</span><span class="legend-bar"><span style="width:%d%%;background:%s"></span></span><span class="legend-val">%d</span></div>`,
			c, template.HTMLEscapeString(it.Kind), percent(it.N, maxV), c, it.N)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// kindBars renders per-kind injection volume for the Retrieval page: one bar per
// memory kind (width normalized to the busiest kind), annotated with the raw
// injection count and how many distinct memories of that kind were surfaced.
func kindBars(rows []store.KindReach) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	maxV := 1
	for _, r := range rows {
		if r.Injects > maxV {
			maxV = r.Injects
		}
	}
	var b strings.Builder
	b.WriteString(`<div class="kindbars">`)
	for _, r := range rows {
		fmt.Fprintf(&b, `<div class="kindbar"><span class="kind">%s</span><div class="bars"><div class="track"><span class="inj" style="width:%d%%"></span></div></div><span class="nums">%d &middot; %d mem</span></div>`,
			template.HTMLEscapeString(r.Kind), percent(r.Injects, maxV), r.Injects, r.Memories)
	}
	b.WriteString(`</div><div class="kindbar-legend"><span><i style="background:var(--brand)"></i> injections</span><span class="kb-note">&middot; distinct memories surfaced</span></div>`)
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Hover readout -- the payload static/charts.js snaps a crosshair to
// ---------------------------------------------------------------------------

// hoverPoint is one datum of a line chart's hover readout: where the crosshair
// snaps to (in viewBox units, so it needs no re-derivation client-side) and the
// already-formatted text the tooltip shows. The phrasing and the local-time
// labels are decided here for the same reason the rest of the chart is: the
// server is the only side that holds the window, the units, and the locale.
type hoverPoint struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Label string  `json:"label"`
	Value string  `json:"value"`
	Sub   string  `json:"sub,omitempty"`
}

// hoverData is a chart's whole hover payload: the viewBox (W x H) the client maps
// pointer pixels through, the Top/Bot the crosshair spans, and the points.
type hoverData struct {
	W   float64      `json:"w"`
	H   float64      `json:"h"`
	Top float64      `json:"top"`
	Bot float64      `json:"bot"`
	Pts []hoverPoint `json:"pts"`
}

// hoverAttr renders the data-hover attribute that arms a chart's hover readout.
// It is pure progressive enhancement -- charts.js is what reads this, and a chart
// still renders whole (with its aria-label, ticks and any <title> fallbacks)
// without it -- so nothing here may change the static drawing. Returns "" when the
// payload cannot be marshaled, dropping the readout rather than emitting a
// broken attribute.
func hoverAttr(w, h, top, bot float64, pts []hoverPoint) string {
	if len(pts) == 0 {
		return ""
	}
	b, err := json.Marshal(hoverData{W: w, H: h, Top: top, Bot: bot, Pts: pts})
	if err != nil {
		return ""
	}
	// json.Marshal already escapes <, > and &; HTMLEscapeString closes the quote
	// (a label is server-formatted, but this attribute is hand-written markup).
	return ` data-hover="` + template.HTMLEscapeString(string(b)) + `"`
}

// ---------------------------------------------------------------------------
// Area chart -- injection trend
// ---------------------------------------------------------------------------

// areaChart renders an injection trend as an SVG line + gradient fill with the
// peak bucket flagged in coral, and up to three ticks below. Points are plotted
// by index; each carries its own pre-formatted (local-time) tick label. Empty
// input renders nothing.
func areaChart(points []store.TrendBucket) template.HTML {
	n := len(points)
	if n == 0 {
		return ""
	}
	const w, h = 560.0, 150.0
	const padT, padR, padB, padL = 14.0, 12.0, 8.0, 12.0
	pw := w - padL - padR
	ph := h - padT - padB

	maxV, peak := 1, 0
	for i, p := range points {
		if p.Count > maxV {
			maxV = p.Count
		}
		if p.Count > points[peak].Count {
			peak = i
		}
	}
	x := func(i int) float64 {
		if n == 1 {
			return padL + pw/2
		}
		return padL + (float64(i)/float64(n-1))*pw
	}
	y := func(v int) float64 { return padT + ph - (float64(v)/float64(maxV))*ph }

	var line strings.Builder
	for i, p := range points {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(i), y(p.Count))
	}
	area := line.String() + fmt.Sprintf("L%.1f %.1f L%.1f %.1f Z", x(n-1), padT+ph, x(0), padT+ph)

	hov := make([]hoverPoint, 0, n)
	for i, p := range points {
		hov = append(hov, hoverPoint{
			X: x(i), Y: y(p.Count), Label: p.Label,
			Value: plural(p.Count, "injection", "injections"),
		})
	}

	alt := fmt.Sprintf("Injection trend across %d periods; peak %d at %s", n, points[peak].Count, points[peak].Label)
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="area"%s>`, hoverAttr(w, h, padT, padT+ph, hov))
	fmt.Fprintf(&b, `<svg viewBox="0 0 %g %g" class="area-svg" style="color:var(--brand)" role="img" aria-label="%s" tabindex="0">`, w, h, template.HTMLEscapeString(alt))
	b.WriteString(`<defs><linearGradient id="areaGrad" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="currentColor" stop-opacity="0.34"/><stop offset="100%" stop-color="currentColor" stop-opacity="0"/></linearGradient></defs>`)
	// baseline gridlines
	for _, g := range []float64{0.25, 0.5, 0.75, 1} {
		yy := padT + ph*g
		fmt.Fprintf(&b, `<line x1="%g" x2="%g" y1="%.1f" y2="%.1f" stroke="var(--border)" stroke-width="1" stroke-dasharray="2 5" vector-effect="non-scaling-stroke"/>`, padL, padL+pw, yy, yy)
	}
	fmt.Fprintf(&b, `<path class="area-fill" d="%s" fill="url(#areaGrad)"/>`, area)
	// No vector-effect here, unlike the gridlines/peak: non-scaling-stroke moves the
	// dash pattern into screen space, which defeats the pathLength="1" normalization
	// the .area-line draw-on animation rides on. The dash then covers 1/scale of the
	// path, silently clipping the line's tail at any width past the 560-unit viewBox.
	fmt.Fprintf(&b, `<path class="area-line" d="%s" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linejoin="round" stroke-linecap="round" pathLength="1"/>`, line.String())
	if n == 1 {
		// A single point has no line/area to draw, and "peak" is meaningless, so
		// render the datum itself as a visible dot in the series color.
		fmt.Fprintf(&b, `<circle class="area-peak" cx="%.1f" cy="%.1f" r="4" fill="currentColor" stroke="var(--surface-solid)" stroke-width="2.5" vector-effect="non-scaling-stroke"/>`, x(0), y(points[0].Count))
	} else {
		// The latest point breathes: the series is live, and its newest value is
		// where the eye should land first.
		fmt.Fprintf(&b, `<g class="area-head"><circle class="area-head-halo" cx="%.1f" cy="%.1f" r="4" fill="currentColor"/><circle cx="%.1f" cy="%.1f" r="3.5" fill="currentColor" stroke="var(--surface-solid)" stroke-width="2" vector-effect="non-scaling-stroke"/></g>`,
			x(n-1), y(points[n-1].Count), x(n-1), y(points[n-1].Count))
		// peak marker
		fmt.Fprintf(&b, `<g class="area-peak"><line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="var(--pop)" stroke-width="1.5" stroke-dasharray="2 3" vector-effect="non-scaling-stroke"/><circle cx="%.1f" cy="%.1f" r="4" fill="var(--pop)" stroke="var(--surface-solid)" stroke-width="2.5" vector-effect="non-scaling-stroke"/></g>`,
			x(peak), x(peak), y(points[peak].Count), padT+ph, x(peak), y(points[peak].Count))
	}
	b.WriteString(`</svg>`)
	// ticks: first / middle / last, using each bucket's own label
	b.WriteString(`<div class="area-ticks">`)
	ticks := []int{0}
	if n > 2 {
		ticks = append(ticks, n/2)
	}
	if n > 1 {
		ticks = append(ticks, n-1)
	}
	for _, i := range ticks {
		fmt.Fprintf(&b, `<span>%s</span>`, template.HTMLEscapeString(points[i].Label))
	}
	b.WriteString(`</div></div>`)
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Coverage trend -- retained-knowledge rate over time
// ---------------------------------------------------------------------------

// coverageTrend renders the windowed session-coverage rate as a line + area fill
// on a fixed 0-100% axis, in the coverage green. The rate is covered/total per
// bucket; buckets with no sessions are backfilled to 0% so a quiet stretch reads
// as a dip in retention rather than an implied ceiling, and only buckets that
// actually had sessions are marked with a dot. It expects a dense bucket series
// (SessionCoverageBuckets); when no bucket in the window had a session it renders
// nothing so the caller can fall back to an empty state.
func coverageTrend(buckets []store.CoverageBucket) template.HTML {
	n := len(buckets)
	if n == 0 {
		return ""
	}
	const w, h = 560.0, 150.0
	const padT, padR, padB, padL = 14.0, 12.0, 8.0, 30.0
	pw := w - padL - padR
	ph := h - padT - padB

	x := func(i int) float64 {
		if n == 1 {
			return padL + pw/2
		}
		return padL + (float64(i)/float64(n-1))*pw
	}
	y := func(pct float64) float64 { return padT + ph - (pct/100)*ph }

	// Coverage rate per bucket; buckets with no sessions count as 0% (backfilled)
	// so the line spans the whole window and quiet buckets visibly drop to the
	// floor.
	pctOf := func(b store.CoverageBucket) float64 {
		if b.Total == 0 {
			return 0
		}
		return float64(b.Covered) / float64(b.Total) * 100
	}
	if !anyCoverage(buckets) {
		return "" // no sessions anywhere in the window
	}

	var line, area, dots strings.Builder
	hov := make([]hoverPoint, 0, n)
	for i, d := range buckets {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(i), y(pctOf(d)))
		if d.Total > 0 { // mark only buckets that actually had sessions
			// The <title> is the no-JS readout: it survives as the fallback for the
			// charts.js hover layer, which masks it with its own capture rect (two
			// tooltips for one point would otherwise stack up).
			fmt.Fprintf(&dots, `<circle class="cov-dot" cx="%.1f" cy="%.1f" r="3.5" fill="var(--ok)" stroke="var(--surface-solid)" stroke-width="2" vector-effect="non-scaling-stroke"><title>%s: %d%% (%d of %d)</title></circle>`,
				x(i), y(pctOf(d)), template.HTMLEscapeString(d.Label), int(pctOf(d)+0.5), d.Covered, d.Total)
		}
		p := hoverPoint{X: x(i), Y: y(pctOf(d)), Label: d.Label}
		// A quiet bucket is drawn at the floor but is NOT 0% retention; say so
		// rather than let the reader take the dip at face value.
		if d.Total == 0 {
			p.Value = "No sessions"
		} else {
			p.Value = fmt.Sprintf("%d%% covered", int(pctOf(d)+0.5))
			p.Sub = fmt.Sprintf("%d of %s retained knowledge", d.Covered, plural(d.Total, "session", "sessions"))
		}
		hov = append(hov, p)
	}
	// One continuous area under the line, pinching to the baseline on empty buckets.
	fmt.Fprintf(&area, "M%.1f %.1f ", x(0), padT+ph)
	for i, d := range buckets {
		fmt.Fprintf(&area, "L%.1f %.1f ", x(i), y(pctOf(d)))
	}
	fmt.Fprintf(&area, "L%.1f %.1f Z", x(n-1), padT+ph)

	var cov, tot int
	for _, d := range buckets {
		cov += d.Covered
		tot += d.Total
	}
	rate := 0
	if tot > 0 {
		rate = int(float64(cov)/float64(tot)*100 + 0.5)
	}
	alt := fmt.Sprintf("Session coverage trend across %d periods; %d%% of %d sessions left a durable artifact", n, rate, tot)
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="area"%s>`, hoverAttr(w, h, padT, padT+ph, hov))
	fmt.Fprintf(&b, `<svg viewBox="0 0 %g %g" class="area-svg" style="color:var(--ok)" role="img" aria-label="%s" tabindex="0">`, w, h, template.HTMLEscapeString(alt))
	b.WriteString(`<defs><linearGradient id="covGrad" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="currentColor" stop-opacity="0.20"/><stop offset="100%" stop-color="currentColor" stop-opacity="0.01"/></linearGradient></defs>`)
	// horizontal gridlines with % axis labels at 100 / 50 / 0
	for _, g := range []struct {
		frac  float64
		label string
	}{{0, "100%"}, {0.25, ""}, {0.5, "50%"}, {0.75, ""}, {1, "0%"}} {
		yy := padT + ph*g.frac
		fmt.Fprintf(&b, `<line x1="%g" x2="%g" y1="%.1f" y2="%.1f" stroke="var(--border)" stroke-width="1" stroke-dasharray="2 5" vector-effect="non-scaling-stroke"/>`, padL, padL+pw, yy, yy)
		if g.label != "" {
			fmt.Fprintf(&b, `<text class="cov-axis" x="%.1f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`, padL-6, yy, g.label)
		}
	}
	fmt.Fprintf(&b, `<path class="cov-fill" d="%s" fill="url(#covGrad)"/>`, area.String())
	fmt.Fprintf(&b, `<path class="cov-line" d="%s" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linejoin="round" stroke-linecap="round"/>`, line.String())
	b.WriteString(dots.String())
	b.WriteString(`</svg>`)
	// axis ticks: first / middle / last of the window
	b.WriteString(`<div class="area-ticks">`)
	ticks := []int{0}
	if n > 2 {
		ticks = append(ticks, n/2)
	}
	if n > 1 {
		ticks = append(ticks, n-1)
	}
	for _, i := range ticks {
		fmt.Fprintf(&b, `<span>%s</span>`, template.HTMLEscapeString(buckets[i].Label))
	}
	b.WriteString(`</div></div>`)
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Bar chart -- events by kind (session detail)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Stacked bar -- task pipeline
// ---------------------------------------------------------------------------

// stackedBar renders the task pipeline as one proportional bar plus a legend.
// Zero-width segments are dropped from the bar but kept in the legend so the
// four-count rhythm stays intact. All-zero renders nothing.
func stackedBar(ready, inProgress, blocked, closed int) template.HTML {
	segs := []struct {
		label string
		n     int
		color string
	}{
		{"Ready", ready, "var(--ok)"},
		{"In progress", inProgress, "var(--brand)"},
		{"Blocked", blocked, "var(--warn)"},
		// A neutral mid-grey, not --surface-3: the latter is ~= the track fill
		// (--surface-2), which made a Closed segment vanish into the empty track.
		{"Closed", closed, "var(--muted)"},
	}
	total := ready + inProgress + blocked + closed
	if total == 0 {
		return ""
	}
	var bar, legend strings.Builder
	for _, s := range segs {
		if s.n > 0 {
			fmt.Fprintf(&bar, `<div class="stackseg" style="width:%.2f%%;background:%s" title="%s %d"></div>`,
				float64(s.n)/float64(total)*100, s.color, s.label, s.n)
		}
		fmt.Fprintf(&legend, `<div class="stackleg"><span class="kdot" style="background:%s"></span><span class="stackleg-l">%s</span><span class="stackleg-n">%d</span></div>`,
			s.color, s.label, s.n)
	}
	return template.HTML(`<div class="stackbar-wrap"><div class="stackbar">` + bar.String() + `</div><div class="stackbar-legend">` + legend.String() + `</div></div>`)
}

// ---------------------------------------------------------------------------
// Sparkline -- the shape behind a vital's headline number
// ---------------------------------------------------------------------------

// spark is a vital card's trailing series. Max pins the y-axis: a RATE always
// plots against 100, so a healthy-but-flat 88% reads flat instead of being
// stretched to fill the box, while a volume (Max 0) normalizes to its own peak
// because its ceiling is not a known quantity.
type spark struct {
	Points []store.TrendBucket
	Tone   string // "" = brand; "ok" = the retention green
	Max    int    // 0 = normalize to the series peak
	Label  string // aria-label; empty renders it decorative
}

// sparkLine renders a spark as a bare 96x26 polyline. Fewer than two points
// renders nothing: a single datum has no shape, and a flat line drawn from it
// would imply a trend that was never measured.
func sparkLine(s spark) template.HTML {
	n := len(s.Points)
	if n < 2 {
		return ""
	}
	const w, h, pad = 96.0, 26.0, 4.0
	maxV := s.Max
	if maxV <= 0 {
		for _, p := range s.Points {
			if p.Count > maxV {
				maxV = p.Count
			}
		}
	}
	if maxV <= 0 {
		maxV = 1
	}
	var pts strings.Builder
	var lastX, lastY float64
	for i, p := range s.Points {
		x := 2 + (float64(i)/float64(n-1))*(w-4)
		v := float64(p.Count)
		if v > float64(maxV) {
			v = float64(maxV)
		}
		y := h - pad - (v/float64(maxV))*(h-2*pad)
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", x, y)
		lastX, lastY = x, y
	}
	// A faint wash under the line and a dot on its newest value: the shape reads
	// as a quantity over time, and the eye lands on "now".
	fill := fmt.Sprintf("2,%g %s %.1f,%g", h, pts.String(), w-2, h)
	cls := "ov2-spark"
	if s.Tone != "" {
		cls += " " + s.Tone
	}
	attrs := ` aria-hidden="true"`
	if s.Label != "" {
		attrs = ` role="img" aria-label="` + template.HTMLEscapeString(s.Label) + `"`
	}
	return template.HTML(`<svg class="` + cls + `" viewBox="0 0 96 26"` + attrs +
		`><polygon class="spark-fill" points="` + fill + `"></polygon><polyline points="` + pts.String() +
		`"></polyline><circle class="spark-head" cx="` + fmt.Sprintf("%.1f", lastX) + `" cy="` + fmt.Sprintf("%.1f", lastY) + `" r="2.2"></circle></svg>`)
}

// ---------------------------------------------------------------------------
// Capture calendar -- the momentum contribution grid
// ---------------------------------------------------------------------------

// calendarGrid renders the momentum capture calendar: one cell per local day,
// weeks as Sunday-first columns, cell intensity from sessions per day, and a
// distinct dot on covered days (a session left a durable artifact). days must
// be contiguous daily buckets beginning at start -- a local midnight, ideally
// week-aligned so the first column is full -- as SessionCoverageBuckets
// produces over a synthetic window; the bucket axis ends at now, so the final
// bucket is today's cell and wears the breathing outline. A day with no
// sessions is an empty cell in the resting tone: a pause, never a warning.
// Each week column is a group carrying a staggered --d delay for the entry
// wave; live morphs preserve these nodes, so the wave plays on a fresh render
// and never on a data patch.
//
// The grid is an instrument, not wallpaper: every cell carries its local date
// as data-day, and charts.js turns that into a hover/keyboard readout and a
// click that focuses the Session map on exactly that day (the ?day filter in
// sessions.go). selected is that filter's current day ("" when unfocused); its
// cell wears the sel outline so the grid shows which day the list is answering
// about. The <title> children stay as the no-JS fallback -- the client lifts
// them into its own readout so the browser tooltip cannot double up.
func calendarGrid(days []store.CoverageBucket, start time.Time, selected string) template.HTML {
	if len(days) == 0 {
		return ""
	}
	const cell, pitch = 11.0, 14.0 // cell size and grid pitch (cell + 3 gap)
	const left, top = 30.0, 16.0   // gutters: weekday labels, month labels
	const wave = 12                // per-column entry-wave delay step, ms

	lead := int(start.Local().Weekday()) // empty rows above the first day, Sunday-first
	weeks := (lead + len(days) + 6) / 7
	maxN := 0
	for _, d := range days {
		maxN = max(maxN, d.Total)
	}

	var cells, months strings.Builder
	lastMonth := time.Month(0)
	lastCol := -1
	day := start.Local()
	for i, d := range days {
		row := (lead + i) % 7
		col := (lead + i) / 7
		x := left + float64(col)*pitch
		y := top + float64(row)*pitch

		if col != lastCol {
			if lastCol >= 0 {
				cells.WriteString(`</g>`)
			}
			fmt.Fprintf(&cells, `<g class="mom-cal-col" style="--d:%dms">`, col*wave)
			lastCol = col
		}

		// Month labels ride the first cell of each new month's week column.
		if row == 0 || i == 0 {
			if m := day.Month(); m != lastMonth {
				fmt.Fprintf(&months, `<text x="%.0f" y="11">%s</text>`, x, m.String()[:3])
				lastMonth = m
			}
		}

		lvl := 0
		if d.Total > 0 && maxN > 0 {
			lvl = min(4, 1+(d.Total*4-1)/maxN)
		}
		title := day.Format("Mon, Jan 02") + " -- no sessions"
		if d.Total > 0 {
			title = fmt.Sprintf("%s -- %s, %d captured", day.Format("Mon, Jan 02"),
				plural(d.Total, "session", "sessions"), d.Covered)
		}
		key := day.Format("2006-01-02")
		cls := fmt.Sprintf("mom-cal-cell l%d", lvl)
		if key == selected {
			cls += " sel"
		}
		if i == len(days)-1 {
			cls += " today"
		}
		fmt.Fprintf(&cells, `<rect class="%s" x="%.0f" y="%.0f" width="%.0f" height="%.0f" rx="2" data-day="%s"><title>%s</title></rect>`,
			cls, x, y, cell, cell, key, template.HTMLEscapeString(title))
		if d.Covered > 0 {
			fmt.Fprintf(&cells, `<circle class="mom-cal-dot" cx="%.1f" cy="%.1f" r="1.8"></circle>`,
				x+cell/2, y+cell/2)
		}
		day = day.AddDate(0, 0, 1)
	}
	cells.WriteString(`</g>`)

	var wdays strings.Builder
	for row, label := range []string{"", "Mon", "", "Wed", "", "Fri", ""} {
		if label == "" {
			continue
		}
		fmt.Fprintf(&wdays, `<text x="0" y="%.0f">%s</text>`, top+float64(row)*pitch+cell-2, label)
	}

	w := left + float64(weeks)*pitch
	h := top + 7*pitch
	// tabindex makes the grid keyboard-walkable (the charts.js readout, same as
	// the trend charts); the aria-label stays the whole-grid summary because the
	// per-day readout is a sighted affordance.
	return template.HTML(fmt.Sprintf(
		`<svg class="mom-cal-grid" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" tabindex="0" role="img" aria-label="Sessions per day over the last year; a dot marks days that captured knowledge. Arrow keys walk the days; Enter focuses the session list on one.">`+
			`<g class="mom-cal-months">%s</g><g class="mom-cal-wdays">%s</g>%s</svg>`,
		w, h, w, h, months.String(), wdays.String(), cells.String()))
}

// ---------------------------------------------------------------------------
// Memory fabric -- composition as one proportional bar
// ---------------------------------------------------------------------------

// kindBar renders the memory-kind composition as a single proportional bar plus
// a counted legend, for the Overview rail. It is the compact sibling of
// kindLegend (which gives each kind its own row): here the question is what the
// knowledge base is made of, so the shares have to be readable against each
// other in one glance rather than against the largest kind.
func kindBar(items []kindCount) template.HTML {
	total := 0
	for _, it := range items {
		total += it.N
	}
	if total == 0 {
		return ""
	}
	sorted := make([]kindCount, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].N != sorted[j].N {
			return sorted[i].N > sorted[j].N
		}
		return sorted[i].Kind < sorted[j].Kind
	})
	var bar, legend strings.Builder
	for _, it := range sorted {
		c := kindColorVar(it.Kind)
		fmt.Fprintf(&bar, `<i style="width:%.2f%%;background:%s" title="%s %d"></i>`,
			float64(it.N)/float64(total)*100, c, template.HTMLEscapeString(it.Kind), it.N)
		fmt.Fprintf(&legend, `<span><i style="background:%s"></i>%s %d</span>`,
			c, template.HTMLEscapeString(it.Kind), it.N)
	}
	return template.HTML(`<div class="ov2-kind"><div class="ov2-kind-bar">` + bar.String() +
		`</div><div class="ov2-kind-legend">` + legend.String() + `</div></div>`)
}

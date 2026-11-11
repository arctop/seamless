package backup

import (
	"io/fs"
	"syscall"
)

// sfDataless is SF_DATALESS (sys/stat.h): the file's contents are not on this
// Mac, only a placeholder an iCloud Drive or File Provider folder downloads on
// first read. Reading such a backup to list it would start that download (and
// block until it ends), so the list shows it without opening it.
const sfDataless = 0x40000000

// isDataless reports whether fi is such a placeholder.
func isDataless(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}

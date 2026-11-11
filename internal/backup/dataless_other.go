//go:build !darwin

package backup

import "io/fs"

// isDataless is macOS's (dataless_darwin.go): elsewhere a listed file's
// contents are on the disk.
func isDataless(fs.FileInfo) bool { return false }

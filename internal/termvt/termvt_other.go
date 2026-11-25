//go:build !windows

package termvt

import "os"

// enable has nothing to turn on: a Unix terminal reads escape sequences as
// they are written.
func enable(*os.File) bool { return true }

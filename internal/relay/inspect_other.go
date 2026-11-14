//go:build !darwin && !linux

package relay

// Supported reports whether a relay runs on this platform: not here, where no
// kernel call names a socket's peer (access.PeerCredSupported). A computer on
// this platform pairs as a key device.
const Supported = false

// inspectPath is unavailable where the relay is.
func inspectPath(string) (FileFacts, error) { return FileFacts{}, ErrUnsupported }

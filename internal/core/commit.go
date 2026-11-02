package core

// IsCommitID reports whether s is a full commit id in its canonical form: the
// object name git prints for a commit, 40 lowercase hex digits (SHA-1) or 64
// (SHA-256). An abbreviation, uppercase hex or anything else is not one; input
// is read leniently by validate.CommitIDs, which lowercases before it checks.
func IsCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

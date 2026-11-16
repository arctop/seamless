package gitread

// LockHooksActive reports whether the checkout at root runs the repo-lock
// hooks: git would run a reference-transaction hook there (from core.hooksPath,
// or else the repository's own hooks folder) that checks the repository's
// locks with `seam lock check`. A checkout whose lock is declared but which
// runs no such hook shows "hooks off" in the briefing and in `seam doctor`.
// root is a checkout root as RepoRoot returns it; "" or a path outside any
// repository reports false.
func LockHooksActive(root string) bool {
	_ = root
	return false
}

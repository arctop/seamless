package config

// Isolation is the isolation block: what project isolation does beyond its
// three states (open, confidential, sealed), which are each project's own
// setting (the console's Overview control, seam project isolation).
//
// Off by default, and off is exactly the previous behavior: a read refused at
// a confidential project's fence is refused, word for word as before, and
// nothing is recorded about it beyond what the fence always recorded.
//
// The yaml block is optional and was added after v0.7.4: a config that carries
// it does not load in a release that predates it (KnownFields), which is why
// the shipped example leaves it commented out.
type Isolation struct {
	// Requests turns a read refused at a confidential project's fence into an
	// access request the owner can approve: a read-only grant of that one
	// project to the one session (under peer_auth, the one agent process tree)
	// that asked, for a limited time. A session in an open project that holds
	// a grant is write-confined to its own project for the rest of its life.
	// Nothing involving a sealed project is ever offered, writes are never
	// granted, and the briefing is unchanged.
	//
	// Without peer_auth the approval is ADVISORY: it is made over the console
	// with the bearer key (seam approve, or the console's Approve), and any
	// process holding the key can make it. Under peer_auth it needs the same
	// proofs as a locked project's lease (the daemon's account through seamlessd
	// approve, a passkey, or a Touch ID key).
	Requests bool `yaml:"requests"`
}

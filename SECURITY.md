# Security policy

## Reporting a vulnerability

Please report security problems privately, through GitHub's private
vulnerability reporting:

**https://github.com/arctop/seamless/security/advisories/new**

Do not open a public issue for a vulnerability. The report stays private
between you and the maintainers until a fix is released, and the advisory
thread is where we reply, ask questions, and agree on disclosure. If you
want credit in the advisory, say how you would like to be named.

Useful details: the Seamless version (`seamlessd version`), your operating
system, whether `peer_auth` or networked mode is in use, and the steps that
show the problem.

## What is in scope

- `seamlessd` and `seam`, including the console, the MCP endpoint, the hooks,
  the relay, and the update and backup machinery
- the installers served at `https://thereisnospoon.org/install` and
  `https://thereisnospoon.org/install.ps1`
- Seamless Approve, the optional macOS Touch ID approver (`macos/SeamApprove`)
- the release artifacts and their signatures

Limits Seamless documents as outside its protection are not vulnerabilities on
their own: for example, a process running as your own account reading
`~/.seamless` when `peer_auth` is off, or root on the same computer. The
[security model](https://thereisnospoon.org/docs/concepts/security/) lists
them. A way around one of the protections it does claim is in scope.

## Supported versions

Fixes ship in a new release. Installs made by the installer update
themselves; anyone else updates with `seamlessd update`. Only the latest
release receives security fixes.

## Verifying a release

Every release's `checksums.txt` carries a Sigstore signature from this
repository's release workflow and, from v0.8.4 on, the maintainer's
hardware-key signature (`checksums.txt.sshsig`). The installers and
`seamlessd update` check them before installing anything, and
`update.release_signers` makes every later update require the maintainer's
signature too. Both are described in
[Update & uninstall](https://thereisnospoon.org/docs/updating/#owner-signed-releases).

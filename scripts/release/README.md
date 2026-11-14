# Cutting a release

A release is built in two places from one config, `.goreleaser.yaml`:

- **The owner's Mac** builds the darwin binaries and the Touch ID approver,
  signs them with the project's Developer ID Application certificate, and has
  Apple notarize them (`macos.sh`).
- **GitHub Actions** (`.github/workflows/release.yml`) builds linux and
  windows and signs `checksums.txt` with Sigstore, into the draft.
- **The owner's hardware key** signs the draft's `checksums.txt`
  (`sign.sh`), and only then does the workflow publish.

The macOS assets reach CI as the tag's draft release. CI accepts them only if
each one matches the sha256 manifest in the owner's signed tag. CI holds no
secret: the `release` environment makes every release wait for the owner's
approval, twice, and the owner pushes the Homebrew cask.

`checksums.txt` lists every archive, the macOS assets and both installer
scripts, so the owner's signature over it (`checksums.txt.sshsig`) covers
every byte a user runs. The keys allowed to make it are `release_signers` at
the repository root, an OpenSSH allowed_signers file in the namespace
`seamless-release`.

## One-time setup of the Mac that releases

1. **The signing certificate.** The project's Developer ID Application
   certificate and its key go in the login keychain. Check:
   `security find-identity -v -p codesigning`
   - When the keychain holds exactly one valid "Developer ID Application"
     identity, `macos.sh` signs with it.
   - Otherwise, set `SEAMLESS_SIGN_IDENTITY` to the identity's SHA-1 hash.
2. **Notarization credentials** in a notarytool keychain profile named
   `seamless-notary`, made from an App Store Connect API key:

   ```bash
   xcrun notarytool store-credentials seamless-notary \
     --key <path to AuthKey_XXXX.p8> --key-id <key id> --issuer <issuer id>
   xcrun notarytool history --keychain-profile seamless-notary   # check
   ```

   - The `.p8` file is no longer needed once the profile holds it.
   - Another profile name works too, through `SEAMLESS_NOTARY_PROFILE`.
3. **The approver's provisioning profile.** This is the Developer ID profile
   for the App ID `com.arctop.seamless.approve`, with Keychain Sharing on.
   Keep it at its default path:

   ```bash
   mkdir -p ~/.config/seamless-release
   mv <downloaded>.provisionprofile ~/.config/seamless-release/approver.provisionprofile
   ```

   - Or set `SEAMLESS_APPROVER_PROFILE` to another path.
   - `macos.sh` reads the team id from this profile, and refuses when the
     signing identity belongs to another team.
   - After the certificate is renewed, regenerate the profile.
4. **The tools.** Xcode (for notarytool and stapler), go and gh (logged in as
   an admin of the repository).
5. **The release key.** A hardware security key enrolled as an OpenSSH
   `sk-ssh-ed25519` (or `sk-ecdsa`) key that asks for its PIN (`ssh-keygen -t
   ed25519-sk -O verify-required`, for example), with its line in both
   `release_signers` (namespace `seamless-release`, for `checksums.txt`) and
   `scripts/release/tag_signers` (namespace `git`, for the tag).
   - **The release signing helper** signs with it: a root-owned program, so no
     process running as you can swap it, named by its absolute path in
     `SEAMLESS_RELEASE_SIGNER` and run under `/usr/bin/env -i`. Its
     `tag <repo> <tag> <sha> [--key K] -F <file>` verb makes the release tag,
     and its `checksums <file> [--key K]` verb writes `<file>.sig` in namespace
     `seamless-release`. Each signature takes the key's PIN, then a touch.
     `SEAMLESS_RELEASE_SIGNER_KEY` picks its `--key` (`owner-yk1` or
     `owner-yk2`).
   - **Without the helper** (a fork): `SEAMLESS_RELEASE_SIGNER=none` makes
     `macos.sh` sign the tag with `git tag -s`, and `SEAMLESS_RELEASE_KEY`
     makes `sign.sh` sign with `ssh-keygen -Y sign` and that key handle file.
     `SEAMLESS_SSH_KEYGEN` names an `ssh-keygen` that can use the key when the
     one on PATH cannot (macOS's own cannot use a FIDO key without a
     middleware). List the key in `tag_signers` either way.
   - Both scripts verify the tag with `/usr/bin/ssh-keygen` against
     `tag_signers` as the tagged tree holds it, never with the `ssh-keygen` on
     PATH and never against your global `allowed_signers`.
   - Check a key's fingerprint against `release_signers`:
     `ssh-keygen -l -f <key>.pub`.

## The release, in order

1. **Prepare.** The prep commit (`docs: prepare vX.Y.Z`) is on main, and CI on
   it is green.
2. **Build the macOS half:** `SEAMLESS_RELEASE_SIGNER=<helper> scripts/release/macos.sh
   vX.Y.Z`. It checks that:
   - the tree is clean;
   - HEAD is on origin/main, and CI is green on it;
   - the tag is on neither origin nor this clone;
   - the certificate and both profiles are present.

   Then it:
   - builds `.goreleaser.yaml`'s builds for darwin, with go.mod's toolchain;
   - signs them (hardened runtime, secure timestamp) and has Apple notarize
     them;
   - packs `seamless_<version>_darwin_{amd64,arm64}.tar.gz`;
   - builds, signs, notarizes and staples the approver
     (`seamless-approve_<version>_darwin.zip`);
   - uploads the three assets to a draft release titled with the tag, and
     reads them back;
   - writes the annotated tag whose message carries their sha256 lines,
     signed by the release signing helper with your hardware key (its PIN,
     then a touch), and checks it against `scripts/release/tag_signers` with
     `/usr/bin/ssh-keygen`.

   Its output is in `dist/macos-<version>/`. Notarization usually takes a few
   minutes.
3. **Push the tag on its own:** `git push origin vX.Y.Z`. Never push it in the
   same command as main.
4. **Approve the build.** The run waits for the owner to approve its
   `release` environment (the goreleaser job).
   - Once approved, it holds the draft's macOS assets to the tag's manifest
     and stops on any difference.
   - Then it builds linux and windows, signs `checksums.txt` with Sigstore,
     and fills the draft. The release stays a draft.
5. **Sign:** `SEAMLESS_RELEASE_SIGNER=<helper> scripts/release/sign.sh vX.Y.Z`,
   in an attended session, while
   the run's publish job waits. `--dry-run` signs and checks but uploads
   nothing. It:
   - verifies your signed tag against `scripts/release/tag_signers` as the
     tag's tree holds it, with `/usr/bin/ssh-keygen`;
   - holds the draft's `checksums.txt` to its Sigstore bundle for this exact
     tag, and its macOS lines to the tag's manifest (`releasekit
     check-draft`);
   - prints `checksums.txt`, the keys that may sign this release, and any
     key this tag adds or removes, and asks you to type the tag;
   - signs `checksums.txt` in namespace `seamless-release` with the release
     signing helper's `checksums` verb (enter the key's PIN, then touch it),
     or with `ssh-keygen -Y sign` and `SEAMLESS_RELEASE_KEY` without the
     helper;
   - checks the signature as the publish job will (`releasekit
     verify-owner`), uploads it as `checksums.txt.sshsig`, and reads it back.
6. **Approve the publish.** The publish job waits for a second approval of the
   `release` environment.
   - It holds `checksums.txt.sshsig` to `release_signers` (below) and stops on
     any failure, the release still a draft. Approved before the signature is
     uploaded, it fails: upload it, then "Re-run failed jobs".
   - Then it publishes the draft, re-verifies every signature on the
     published bytes (the Sigstore pair and bundle against the exact tag
     identity, and the owner's), publishes to the MCP registry, and publishes
     the site from the tag (`pages.yml`).
7. **The cask:** `scripts/release/cask.sh vX.Y.Z`. Add `--dry-run` first to
   see the change.
   - It checks the release's `checksums.txt` against its Sigstore bundle for
     this exact tag, and against the owner's signature as the publish job
     does.
   - It renders the cask as goreleaser did, and pushes it to
     `arctop/homebrew-tap` with your login.
   - A prerelease gets no cask.
8. **The changelog:** `make changelog`, then commit and push.
   - A main push no longer reaches the site, so publish the refreshed site
     with a `site-*` tag:

     ```bash
     git tag -s site-YYYYMMDD -m site-YYYYMMDD && git push origin site-YYYYMMDD
     ```

   - Then `make indexnow`.

## Which keys may sign: `release_signers`

The publish job (`releasekit verify-owner -require`) reads `release_signers`
from git at two tags: **the previous release's** (the newest `vX.Y.Z` tag below
this one; prerelease tags never count) and **this one's**. The signing key must
be listed, for `seamless-release`, in both.

- **A key added in a release signs from the next release on.** Whoever can push
  a commit to main can add a line, but the release that carries the line
  cannot be signed by its key: the previous tag does not list it. `sign.sh`
  and the publish job print every added key as `ADDED`, so the owner sees it
  before signing with another key.
- **A key removed in a release stops signing at once**, because this tag no
  longer lists it. To replace a key: add the new one in one release (signed
  with the old one), then remove the old one in the next (signed with the new
  one).
- **Two files with no key in common are refused**, and so is a release that
  deletes `release_signers`: a release cannot turn the check off.
- Never put `verify-required` on a line: `ssh-keygen` refuses the line. The
  verifiers require user verification (flags `0x05`, touched and unlocked)
  from the signature itself.

**The bootstrap.** The first release that carries `release_signers` has no
previous tag holding the file (v0.8.3 has none), so the publish job holds it
to **that release's own file**, and says `BOOTSTRAP` in the job log, in
`sign.sh` and in `releasekit owner-keys`. During the bootstrap, whoever could
push the commit that adds `release_signers` decides which keys sign that one
release; the owner checks each fingerprint in the job log against the keys
themselves (`ssh-keygen -l -f <key>.pub`) before approving. From the next
release on, the keys come from a tag the owner signed and published.

**What the release-side check is worth.** The workflow runs from the tag being
released, so whoever can push a `v*` tag (an admin, or a token that can) can
push a commit whose `release.yml` skips the check, and approve the
environment too. The check keeps the normal path honest: no release publishes
without the owner's hardware-key signature, and a key slipped into main cannot
sign the release that carries it. Against a hostile tag pusher, the guarantees
are the clients':

- an updater pinned with `update.release_signers` installs nothing the
  owner's keys did not sign ([the guide](https://thereisnospoon.org/docs/updating/#owner-signed-releases));
- `docs/install` and `docs/install.ps1` verify `checksums.txt.sshsig` against
  the keys they carry, for every release after v0.8.3 (`scripts/site-check.sh`
  holds those copies to `release_signers`). Their trust root is the site
  that serves them, which also publishes from release tags;
- `cask.sh` runs on the owner's machine and checks the signature before it
  renders the cask.

**Dry runs.**
- `scripts/release/macos.sh vX.Y.Z --dry-run` does everything up to the
  draft, signing and notarization included. It uploads nothing and makes no
  tag, and only warns where a release would stop.
- Add `--adhoc` to skip the certificate and Apple: the binaries are ad-hoc
  signed, and there is no approver.
- `make release-snapshot` is goreleaser's dry run of the linux and windows
  half.

**Running it again.**
- The draft and its assets are reused and replaced.
- A local tag from an earlier run is refused: it was never pushed, so
  `git tag -d vX.Y.Z` and run again.
- A tag already on origin is final.

## Forks

A fork's release is not held to the owner's signature: on any repository but
arctop/seamless the publish job publishes the draft as soon as the goreleaser
job has filled it (the fork's `release` environment is unprotected unless the
fork protects it). The installers carry this repository's keys, so a fork that
serves its own installers replaces their `release_signers` block (and the
repository's `release_signers`) with its own keys, or empties it.

A fork's tag with no manifest in its message releases linux and windows only,
with no darwin archives. To ship macOS archives too, run
`macos.sh vX.Y.Z --adhoc` in the fork's clone. It uploads ad-hoc-signed
archives to the fork's draft and writes the manifest tag, and the fork's
release.yml checks them the same way. Without a Developer ID they are not
notarized, and there is no approver. On arctop/seamless, `--adhoc` runs only
as a dry run.

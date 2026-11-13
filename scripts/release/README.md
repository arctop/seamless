# Cutting a release

A release is built in two places from one config, `.goreleaser.yaml`:

- **The owner's Mac** builds the darwin binaries and the Touch ID approver,
  signs them with the project's Developer ID Application certificate, and has
  Apple notarize them (`macos.sh`).
- **GitHub Actions** (`.github/workflows/release.yml`) builds linux and
  windows, signs `checksums.txt` with Sigstore, and publishes.

The macOS assets reach CI as the tag's draft release. CI accepts them only if
each one matches the sha256 manifest in the owner's signed tag. CI holds no
secret: the `release` environment makes every release wait for the owner's
approval, and the owner pushes the Homebrew cask.

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
4. **The tools.** Xcode (for notarytool and stapler), go, gh (logged in as
   an admin of the repository), and git set up to sign tags (`gpg.format ssh`)
   and to verify them (`gpg.ssh.allowedSignersFile`, so `git tag -v` works).

## The release, in order

1. **Prepare.** The prep commit (`docs: prepare vX.Y.Z`) is on main, and CI on
   it is green.
2. **Build the macOS half:** `scripts/release/macos.sh vX.Y.Z`. It checks that:
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
   - writes the SSH-signed annotated tag whose message carries their sha256
     lines.

   Its output is in `dist/macos-<version>/`. Notarization usually takes a few
   minutes.
3. **Push the tag on its own:** `git push origin vX.Y.Z`. Never push it in the
   same command as main.
4. **Approve.** The run waits for the owner to approve its `release`
   environment.
   - Once approved, it holds the draft's macOS assets to the tag's manifest
     and stops on any difference.
   - Then it builds linux and windows, signs `checksums.txt`, and publishes
     the draft.
   - Then it re-verifies the signatures, publishes to the MCP registry, and
     publishes the site from the tag (`pages.yml`).
   - (Step D will add the owner's hardware-key signature over `checksums.txt`
     between CI's draft and the publish.)
5. **The cask:** `scripts/release/cask.sh vX.Y.Z`. Add `--dry-run` first to
   see the change.
   - It checks the release's `checksums.txt` against its Sigstore bundle for
     this exact tag.
   - It renders the cask as goreleaser did, and pushes it to
     `arctop/homebrew-tap` with your login.
   - A prerelease gets no cask.
6. **The changelog:** `make changelog`, then commit and push.
   - A main push no longer reaches the site, so publish the refreshed site
     with a `site-*` tag:

     ```bash
     git tag -s site-YYYYMMDD -m site-YYYYMMDD && git push origin site-YYYYMMDD
     ```

   - Then `make indexnow`.

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

A fork's tag with no manifest in its message releases linux and windows only,
with no darwin archives. To ship macOS archives too, run
`macos.sh vX.Y.Z --adhoc` in the fork's clone. It uploads ad-hoc-signed
archives to the fork's draft and writes the manifest tag, and the fork's
release.yml checks them the same way. Without a Developer ID they are not
notarized, and there is no approver. On arctop/seamless, `--adhoc` runs only
as a dry run.

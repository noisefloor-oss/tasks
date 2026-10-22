# Packaging the noise tasks compatibility release

`scripts/package.sh` builds all command surfaces with one recorded libexec
root. Because the package contains a setuid broker, the script runs only as
root and fails unless it can install and verify the broker as a regular
root:root mode-6755 file under trusted ancestry. `PREFIX` selects the recorded
root; `DESTDIR=/` selects a live installation (`make package` is a convenience
wrapper):

```sh
sudo env VERSION=1.0.0 PREFIX=/usr/local DESTDIR=/ sh scripts/package.sh
sudo env VERSION=1.0.0 PREFIX=/usr DESTDIR=/ sh scripts/package.sh
```

The corresponding roots are `/usr/local/libexec/noise` and
`/usr/libexec/noise`. A live prefix whose ancestry is non-root-owned or
group/world-writable is refused; this includes ordinary user-owned Homebrew
prefixes because they cannot safely host a setuid-root helper. The package contains
`noise`, `noise-tasks`, and the transition-only `noisetask` shim in
`PREFIX/bin`.
It also contains the private plugin payload in `PREFIX/lib/noise/payload` and
the build-bound publisher at `PREFIX/libexec/noise-setup`. Direct Go builds
retain the defaults root `/usr/local/libexec/noise` and version `dev`.

For a staged package, create a private root-owned mode-0700 staging root first
and pass it explicitly. The script refuses a missing, symlinked, non-root-owned,
or broader staging root, as well as replaceable ancestry (a root-owned sticky
temporary ancestor is permitted), and verifies the installed broker before
succeeding:

```sh
sudo install -d -o root -g root -m 0700 /var/lib/noise-package-root
sudo env VERSION=1.0.0 PREFIX=/usr DESTDIR=/var/lib/noise-package-root \
  sh scripts/package.sh
```

The package does not install a project-selected provider executable. The
`anthropic-api` adapter is built into `noise-tasks`; the allowlisted
`claude-cli`, `omp`, `codex`, and `kimi` adapters use fixed executable basenames
from your trusted `PATH` and reject a binary contained by the project boundary.
`.tasks/preferences.json` can select only an allowlisted adapter and model,
never a path or argument vector. You must install and authenticate each CLI
separately and should test it with the same runtime identity used by agents.

The payload directory is not the live plugin root. A package post-install or
upgrade step creates a mode-0700 staging directory directly beneath
`PREFIX/libexec`, copies only the complete payload into it with executable
owner-only modes, and invokes the publisher. For example, a root-owned system
package with an agent running as UID 1200 and GID 1200 uses:

```sh
install -d -o 0 -g 0 -m 0755 /usr/libexec
stage=$(mktemp -d /usr/libexec/.noise-stage.XXXXXX)
install -o 0 -g 0 -m 0700 /usr/lib/noise/payload/noise-tasks \
  "$stage/noise-tasks"
/usr/libexec/noise-setup --stage "$stage" \
  --setup-uid 0 --runtime-uid 1200 --runtime-gid 1200
```

Every `--setup-uid` is an installation owner; UID 0 is always included because
Unix cannot isolate an ordinary installation from effective UID 0. A
`--setup-gid` declares a group that may write a validated ancestor. Repeatable
`--runtime-uid` and `--runtime-gid` arguments name agent identities that must
not own or write any component. Setup refuses overlap between setup authority
and runtime identities.

For Homebrew, pass `--homebrew`, declare the prefix owner with `--setup-uid`,
and declare every group that can write a prefix ancestor with `--setup-gid`.
This is accepted only for `/opt/homebrew/libexec/noise` or the Homebrew-managed
`/usr/local/libexec/noise`. An agent running as that same owner cannot be
claimed as isolated; use a distinct runtime identity for autonomous agents.

The publisher takes an exclusive sibling lock and revalidates under it. It
checks the recorded and canonical ancestry, declared ownership and writable
groups, runtime identity separation, mode bits, ACLs, local mount policy,
staging privacy, entry grammar, symlink containment, regular files, and
executable modes. It synchronizes the staged files, freezes the tree, renames
it to an immutable sibling version, then atomically replaces the recorded root
symlink and synchronizes the parent directory. An old version is retained so
a dispatcher that already resolved it can finish safely. A refusal identifies
both the rejected path and trust condition; failures before publication and
recoverable failures after the link swap preserve the prior root and version
bytes. A process crash observes either the old complete root or the new
complete root, never a partially updated tree.

Linux mount validation is fail-closed. The supported writable, executable
filesystem types are ext2/ext3/ext4, XFS, Btrfs, F2FS, NILFS2, bcachefs,
OpenZFS, JFS, ReiserFS, JFFS2, overlayfs, and AUFS. Overlayfs and AUFS support
containerized package construction and installation while retaining the same
same-filesystem rename requirement. Read-only or `noexec` mounts are refused,
as is every unlisted type, including NFS, SMB/CIFS/SMB2, Ceph, other clustered
filesystems, and unknown future magic values. A refusal reports the path and
numeric filesystem type so a legitimate new local filesystem is visible and
can be added deliberately rather than silently treated as trusted. On macOS,
setup requires the kernel's local-mount flag and refuses read-only mounts.

`noise-setup` has no option or environment variable for changing its embedded
root. Packaging a different prefix requires rebuilding all four artifacts with
the same linker-selected value. Normal `noise` and `noisetask` execution only
perform bounded root/plugin resolution and never repeat setup's ancestry or
principal audit. Any change to setup principals, runtime identities, or the
selected prefix is applied only by staging the complete payload again and
running a fresh validation/publication transaction.

## Confined-registration broker

`scripts/package.sh` also builds the §9.5 confined-registration broker as
`PREFIX/libexec/noise-worktree-broker`, installed setuid+setgid root
(mode 6755), and the unprivileged host-side minting tool
`PREFIX/libexec/noise-broker-mint` (mode 0755). Both live beside the
recorded libexec root, outside the version-swapped plugin tree the
publisher manages. Packaging refuses unless the broker is a regular
root:root mode-6755 file and its live installation ancestry is root-owned
and not group/world-writable (or its staging root is private root-owned mode
0700). Deployment installs the 32-byte capability key as root
at `PREFIX/libexec/noise-worktree-broker.key` (mode 0400); see
`docs/BROKER.md` for the capability format and the Deck-side controller/leaf
confinement boundary.

## Building from source

Requires the Go version declared in `go.mod` or newer. The build is pure Go,
including the SQLite driver, so no C toolchain or system libraries are
needed. `make build VERSION=X.Y.Z` produces the standalone engine
`build/bin/noise-tasks`, the `noise` umbrella host and the transition-only
`noisetask` shim in `build/bin/`, and the plugin payloads under
`build/libexec/`. A plain `make build` without `VERSION` embeds the
development version `dev`. `make package` assembles the full system
installation — umbrella host, managed plugin root, and the setuid worktree
broker used by sandboxed orchestration — and requires root by design.

Release artifacts are published on GitHub Releases with SHA-256 checksums.
Verify downloads against the checksums file attached to the release.

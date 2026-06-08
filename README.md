Kine (Kine is not etcd)
=======================

Kine is an etcdshim that translates etcd API to:
- SQLite
- Postgres
- MySQL/MariaDB
- NATS

## Features
- Can be ran standalone so any k8s (not just K3s) can use Kine
- Implements a subset of etcdAPI (not usable at all for general purpose etcd)
- Translates etcdTX calls into the desired API (Create, Update, Delete)

See an [example](/examples/minimal.md).

## About this fork

`loft-sh/kine` is a fork of [`k3s-io/kine`](https://github.com/k3s-io/kine) maintained by vCluster Labs.
On top of upstream it adds:

- AWS RDS IAM authentication for PostgreSQL
- Kerberos/GSSAPI authentication for PostgreSQL

Fork images are published to `ghcr.io/loft-sh/kine`. The Go module path stays `github.com/k3s-io/kine` so the
fork is a drop-in replacement for upstream.

## Fork policy

The fork follows a strict branching, commit, and release model so the delta against upstream stays small and
reviewable, and upstream syncs stay clean. Most of this is enforced automatically (see "What CI enforces").

### Branch model

- `master` tracks upstream `k3s-io/kine`. We do not build releases from `master`.
- Each release line lives on its own branch `release/vcluster/vX.Y.Z`, cut from the matching upstream release
  tag `vX.Y.Z`, with the fork commits replayed on top.

### Commit conventions

All commits use [Conventional Commits](https://www.conventionalcommits.org/). Fork-specific changes carry the
literal scope `fork`:

```
feat(fork): add kerberos gssapi auth for postgresql
fix(fork): close mysql connection after create database
ci(fork): apply loft-sh fork release infrastructure
```

Two rules apply depending on the target branch of a pull request:

- **PRs into `master`/`main`** may not contain product `(fork)` commits (`feat(fork)`, `fix(fork)`,
  `perf(fork)`, `refactor(fork)`). Those belong on a `release/vcluster/v*` branch. Tooling `(fork)` commits
  (`ci`, `chore`, `docs`, `style`, `test`, `build`, `revert`) are allowed only when every changed file is in
  the tooling allowlist: `.github/**`, `updatecli/**`, `docs/**`, `README.md`, `CONTRIBUTING.md`,
  `Dockerfile.release`, `.golangci.json`. Plain upstream-sync commits (no `fork` scope) are always allowed.
- **PRs into `release/vcluster/v*`** must consist solely of Conventional Commits that carry the `fork` scope.

### Releases and versioning

Fork releases are tagged `vX.Y.Z.vcluster.N`, where `vX.Y.Z` is the upstream release the line is based on and
`N` increments per fork release on that line. The first fork release of upstream `v0.16.2` is
`v0.16.2.vcluster.0`, the next is `v0.16.2.vcluster.1`, and so on.

There are two release workflows:

- `release.yaml` (workflow name `Kine build`) fires on a version **tag push** and publishes the multi-arch
  image `ghcr.io/loft-sh/kine:<tag>` (with SBOM and provenance attestation).
- `release.yml` (inherited from upstream) fires when a **GitHub Release is created** and uploads multi-arch
  binaries plus SHA256 checksums to that Release.

In parallel, `version-policy.yaml` validates the pushed tag against the `vX.Y.Z.vcluster.N` scheme and fails
its check if the tag does not conform, so a mis-named release tag is surfaced rather than published silently.

To cut a new release line from a fresh upstream version:

```bash
# 1. Make sure the upstream remote and tags are available
git remote add upstream https://github.com/k3s-io/kine.git   # once
git fetch upstream --tags

# 2. Branch from the upstream release tag (NOT from master)
git checkout -b release/vcluster/v0.16.2 v0.16.2

# 3. Replay ALL fork commits from the previous release line (see "Replay rule")
#    plus the loft-sh CI/release tooling snapshot, then commit.

# 4. Push the branch and the first fork tag
git push origin release/vcluster/v0.16.2
git tag v0.16.2.vcluster.0
git push origin v0.16.2.vcluster.0          # fires release.yaml + version-policy.yaml

# 5. Create the GitHub Release from the tag             # fires release.yml (binaries)
```

For a follow-up release on an existing line, add the fix commits to the branch and push the next tag
(`v0.16.2.vcluster.1`).

#### Replay rule

A new `release/vcluster/vX.Y.Z` branch starts from a clean upstream tag and therefore carries none of our work.
When you cut it you must replay the **complete set of `(fork)`-scoped commits** the previous release line
carried (for example AWS RDS IAM auth and Kerberos/GSSAPI auth), plus the loft-sh CI/release tooling snapshot,
on top of the new upstream base. Bumping to a newer upstream version never drops fork changes, it only rebases
them forward. The previous release branch is the authoritative inventory of what must be replayed:

```bash
# List the fork commits carried by the previous release line
git log --grep '(fork)' release/vcluster/<previous-version>
git log --oneline v<previous-upstream>..release/vcluster/<previous-version>
```

### What CI enforces

- **`fork-policy.yml`** runs on every pull request and applies the per-branch commit rules above: it rejects
  product `(fork)` commits on PRs into `master`/`main`, rejects tooling `(fork)` commits there that touch
  non-allowlisted paths, and on PRs into `release/vcluster/v*` requires every commit to be a Conventional
  Commit with the `fork` scope.
- **`version-policy.yaml`** runs on every version tag push and fails if the tag does not match
  `vX.Y.Z.vcluster.N`. It runs alongside `release.yaml` (which is left as-is) rather than gating it, so a
  mis-named tag turns the check red instead of being blocked outright.

Cutting the `release/vcluster/v*` branch from the correct upstream tag and replaying the full fork commit set
(the replay rule) is not automated and remains a human responsibility.

## Developer Documentation

A high level flow diagram and overview of code structure is available at [docs/flow.md](/docs/flow.md).

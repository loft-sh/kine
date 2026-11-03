# Kine release repair

Owner: Piotr Zaniewski. Issue: DEVOPS-1802. Evidence date: 3 November 2026.
This change prepares the publication barrier and repair procedure. No image tag,
GitHub Release, deployment, or customer communication is changed by it.

## Publication contract

| Stage / trigger | Reads | Mutations | Emits / skip and remedy |
| --- | --- | --- | --- |
| Version tag push | Tagged tooling snapshot and all destination version tags | None, new shared preflight | Continue only after GHCR confirms every tag absent; lookup failure stops the run |
| Existing per-tag job | Source and build dependencies | Existing binary build and draft creation/upload | Existing published Release stops the run; draft retries require absent images |
| Existing image publication | Built source and complete destination list | Existing version and rolling image pushes, then attestations | Partial push blocks retry; cut a new tag or request scoped recovery |
| Existing release finalization | Images, attestations and binary assets | Existing draft publication | Consumers get a new release; existing version tags are never rebuilt |

Failure overlay: tag overwrite and ambiguous registry errors are NEW and covered
by `ghcr-tag-guard`; same-tag job overlap is COVERED by existing concurrency.
Interrupted normal/nocgo publication is PARTIAL: the guard prevents another
write, but recovery requires an operator decision. Upstream binary selection is
PARTIAL: building from the tagged checkout removes the old clone path; consumer
behavior still needs binary and QE checks.

Existing systems: `release.yml` owns the draft, build, attestation and final
publication. `version-policy.yaml` validates the fork version. The shared action
owns GHCR read/error handling. `release-guard-test.yml` tests the real publisher
and future tooling snapshots. Both legacy triggers and the upstream-cloning
`Dockerfile.release` are removed from master.

Observability: the preflight log names the tag, existing digest and refusal
reason. A failed check produces a failed release job and no subsequent release
steps. No new service metric or alert route is introduced. Capture the run URL
with the release evidence; do not treat a green unit test as a published image.

All current branch heads must carry the guard before use. Merge and publish the
shared `ghcr-tag-guard/v1` action before using the kine caller changes. Apply the
four maintenance PRs and the master tooling snapshot. A new line starts from its
upstream base, so replay the guarded tooling and run the contract test before
cutting a tag. These PRs do not modify historical workflow revisions: never rerun
an old unguarded publisher to repair a tag.

The preflight is not an atomic registry reservation. Same-tag publishers must
share `release-<tag>` concurrency within this repository. Manual writers and
other repositories bypass it. GitHub immutable Releases and native GHCR controls
are separate work under DEVOPS-1792.

## Snapshot and candidate review

The read-only refresh matched all three digests recorded in DEVOPS-1802. Preserve
the complete current index JSON locally before an approved write. Use immutable
digests for every source, binary, and provenance check.

The guard refused each of the three existing tags twice, naming its current
digest. `crane digest` before and after each pair of attempts returned the same
value. A unique absent version tag passed the live read-only check without
publishing it. Offline tests cover a different existing digest, partial variant
publication, retries, authentication/API failures, and malformed responses.

| Tag | Current index | Proposed fork index | Expected source |
| --- | --- | --- | --- |
| `v0.14.16.vcluster.2` | `sha256:e41513a89a66a84af0e8d71f03fb7dabb78d0976e08f6d356f493a0c51a7a6a0` | `sha256:5c66fe4e5ed427bbed756e39e6b7f6b847634d6c9e22a8ac7beee00abb68be63` | `99f2be1e0125ceac0601b569c5c8ed3d30014472` |
| `v0.14.16.vcluster.4` | `sha256:0675df8bb412a240881da267e36e537728f34a26489db4816f5f7c52656fabb2` | `sha256:4ed2cd65317bc5d472ac06e922853dad77d0c79bd5a0699f8ac0f0e1ccc41759` | `c56253a9ac18635d00239f7d760dbcdce6bdc9bb` |
| `v0.16.3.vcluster.1` | `sha256:98181c2a0a622728dff1a2a2279453f7c4f21c160ffbc40fcde1cd3845527e35` | `sha256:35ef05e79272d7dad32216e51d19adc0f201cfd7e6d2b837ae700c6fb05c472f` | `dc87832df1750b32a63246af39156c659c31bf17` |

Current indexes list linux/amd64 and linux/arm64. Candidate indexes list both
plus linux/arm/v7. `unknown/unknown` descriptors are attestation manifests.
Architecture descriptors do not prove binary or runtime behavior.

All nine candidate binaries were extracted by immutable index digest and read
with `go version -m`, without executing them. Their GOARCH/GOARM, embedded fork
version, and full source revision match the table on all three architectures.
Every binary reports `vcs.modified=true`; this is not source-equivalence or
reproducibility proof. The `.2` binaries use Go 1.24.13; the other two use Go
1.26.10. Metadata and preserved index manifests remain local evidence under
`/tmp/devops-1802-evidence/` and must be retained with the approved repair record.

Candidate `.4` and `v0.16.3.vcluster.1` passed GitHub attestation verification to
the expected revisions and `release.yml` runs 39970776325 and 39975266795.
Candidate `.2` still returns HTTP 404 from GitHub attestation verification.
Its embedded BuildKit statement and matching binary metadata are evidence, not
a GitHub-verified signature. Engineering and QE must accept that limitation or
choose a newly verified replacement. The historical `.2` Kerberos failure in
ENGCP-1011 makes behavior tests necessary regardless of version strings.

## Approval and repair procedure

Piotr must record approve or decline for each tag, including exact old/new
digests, reviewer acceptance, the QE owner, and rollback authority. No such
approval was found in the issue comments or the reviewed dependency thread.
Until then, perform only the read-only checks.

For each approved tag, confirm no publisher is running. Recheck the current
digest immediately before writing and stop if it differs from the approved
snapshot. Retag the whole candidate index only within the exact recorded scope.
Do not change `latest`, nocgo tags, Git tags, or Platform tags. After each write,
require the tag digest to equal the approved candidate, inspect every runnable
architecture, and repeat binary and provenance checks by immutable digest.
Stop all remaining writes if any check fails.

If a tag is declined, record the supported digest-pinned replacement and how
consumers of the misleading tag will be informed. A retag alone does not change
the kine binary already baked into a Platform image or restart a deployment.

Rollback owner is Piotr. Rollback needs an explicit recorded decision and a
fresh check for intervening writers. Its permitted scope is only the approved
tag, restored to its recorded old whole-index digest. Those old images contain
the known upstream mismatch. Restoring a registry tag does not restore an
already built product or deployment. Stop and revise approval if the baseline
changed. The issue retains the exact proposed repair and rollback commands;
they are not automatic recovery steps.

## Platform and QE acceptance

Keep source, released artifact, deployed version, and runtime acceptance separate.
The issue records an upstream binary in the current `4.10.3` amd64 image and fork
metadata in `4.10.9`, `4.11.4`, and `4.12.5`. That evidence is not a completed
upgrade test or a customer exposure assessment.

The accepted release and QE owners must record the disposition of each supported
line. Test `4.10.3` to the existing `4.10.9` artifact by immutable digest before
choosing it as the supported upgrade. If unsuitable, ship a new patch; never
replace `4.10.3`. For `4.11` and `4.12`, decide whether the next scheduled patch
is sufficient. Any new bump changes `KINE_VERSION` and `KINE_DIGEST` together in
both Platform Dockerfiles and passes the existing pin verifier. Rebase the
unpinned auxiliary `release-4.10-bump-vcluster-0.35.4` branch before release use.

QE evidence must include PostgreSQL with Kerberos/GSSAPI authentication, startup
and leader election, existing data reads and writes before and after upgrade,
restart persistence, and supported recovery behavior. Record exact Platform and
kine digests, architecture, test setup, command/run URL, expected result, actual
result, and QE sign-off. Do not run an unapproved production upgrade or claim
metadata inspection covers these tests.

Piotr, QE, the release owner, and Support must agree the actual exposed versions
and installations, supported target, timing, rollback/support guidance, audience,
sender and lasting reference, or a reasoned no-outreach decision. No installation
inventory or causal customer incident is claimed here. Piotr/triage owns any
later message draft. The Notion Forks policy update and broader SOC2 rollout
remain under DEVOPS-1792; merging these PRs does not complete that work.

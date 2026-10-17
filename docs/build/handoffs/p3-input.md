# Phase 3 handoff: untrusted input

Phase 3 security review of untrusted input ([15](../../spec/15-build-plan.md), "Phase 3: Hardening"), on branch `ws/p3-input` from `382cc88`. The threat model is to be written from this file.

## What was reviewed

| Area | Code | Questions asked |
|---|---|---|
| Repo paths | `internal/model` (`CleanRepoPath`), `internal/repo` (Commits, tree cache, zip export), `internal/hfapi` and `internal/api` callers | `../`, absolute paths, backslashes, NUL, names a client checks out as `.git`, very long paths, Unicode normalisation, case collisions |
| git push and Pull packs | `internal/gitserver/receive.go`, `internal/repo/applypack.go`, `internal/gitstore` | Huge packs, delta bombs, zip-bomb objects, tree fan-out, deep trees, long paths, many refs, many objects, malformed packs, push options |
| Upstreams | `internal/upstream/client.go`, `gitfetch.go`, Pull-through and Discover callers | SSRF to private, loopback, link-local and metadata addresses, DNS rebinding, redirects, scheme and port changes, unbounded ref advertisements |
| Webhooks | `internal/webhook/service.go` | The same SSRF questions, redirects, `webhooks.allow_private_targets` |
| Import and push from disk | `internal/cli` (`push`, `import --from-dir`, `--from-hf-cache`, `--from-lmstudio`, `--from-ollama`, `download`), `internal/upstream/importer` | Symbolic links out of the source folder, paths from the server written to disk |
| git-lfs | `internal/gitserver/lfs.go` | Batch size and body limits (present: 1,000 objects, 4 MiB) |
| Xet | `internal/xet` | Phase 2 residual W11 (index CPU amplification) |
| Pulls | `internal/upstream/plan.go` | Phase 2 residual W6 (cross-Namespace starvation through OID-deduplicated `pull.file` Jobs) |
| Dependencies | `go.mod`, `ui/pnpm-lock.yaml`, `compat/uv.lock`, `docs/requirements.txt` | Known CVEs |

## Findings

Severity is for a self-hosted Instance where any signed-in user can create a Repo in their own Namespace (open or invite registration).

| # | Severity | Finding | Where | Fix | Tests |
|---|---|---|---|---|---|
| 1 | High | **Delta bomb.** go-git rebuilds every delta in memory before the pre-receive hook runs, and `git index-pack` does the same. A 16 KiB pushed pack whose delta copies a 16 MiB base 64 times made the server allocate 1,064 MiB of heap; more copies take any amount. Any user who can push to any Repo (their own) could take the web process down. Pulls had the same exposure to a hostile Upstream through `git index-pack`. | `internal/gitserver/receive.go`, `internal/repo/applypack.go` | `dc3918b`: `gitstore.LimitReceivePack` and `LimitPack` read the pack as it streams and hold back its 20-byte trailer until every object passes, so neither go-git nor `index-pack` finishes the pack (and starts rebuilding deltas) before the check. Limits in [04](../../spec/04-git.md) "Pack and tree limits". | `TestPushRefusesDeltaBomb` (peak heap under 256 MiB, message names the limit), `TestApplyPackRefusesDeltaBombs` (push and Mirror), `TestLimitPackRefusesHostilePacks`, `TestLimitPackPassesValidPacksThrough`, `TestLimitPackRefusesMalformedPacks`, `TestLimitPackDeltaLimitIsSeparate`, `TestLimitPackPassesOnSourceErrors`, `TestLimitPackCloseStopsTheCheck`, `TestPushWithOptionsAndDeltas` |
| 2 | High | **Tree fan-out bomb.** git stores a folder once however often a tree names it. A 666-byte pack whose Commit names one subtree 16 times per level was accepted; listing it (which the `index` Job does for every new head) took 45 seconds, 1.36 GiB of heap and 2,167,056 tree cache rows at 16^5 files. At 16^7 it does not fit in memory. Pulls were exposed the same way. | `internal/repo/applypack.go`, `internal/repo/cache.go` | `dc3918b`: `checkTrees` reads each distinct tree of every Commit in a pack once and refuses Commits over 1,000,000 paths or with a path over 4,096 bytes. The tree cache streams `ls-tree` and stops at the same limits, so Commits stored before the fix fail to list rather than fill memory. `aa8a2ce`: server-side Commits are bounded the same way. `2188682` and `bdd53d3`: a small server-side Commit starts no git process for the check, and the check walks trees with one `cat-file --batch` process, refusing commit and tree objects over 64 MiB before reading them. | `TestApplyPackRefusesTreeBombs` (16^7 files in under 1 KiB, refused in well under 30 s, push and Mirror), `TestApplyPackPathLimitIsConfigurable` (limit, legacy Commit read, server-side Commits) |
| 3 | Medium | **Names that reach `.git` or escape a folder.** `CleanRepoPath` refused only the exact name `.git`, so the commit API accepted `.GIT/config`, `git~1/config` and HFS+ spellings such as `.g‌it/config`, which git clients on Windows and macOS check out into `.git` (CVE-2014-9390) or refuse, leaving the Repo unclonable. Pulls ran `index-pack` without `--strict`, so an Upstream could store `..`, `.git` or backslash names, which the Hub API then listed and which extract outside a client's folder. Paths had no length limit. | `internal/model/entry.go`, `internal/repo/applypack.go` | `dc3918b`: `model.ValidPathComponent` ports git's `is_ntfs_dotgit` and `is_hfs_dotgit` and refuses empty, `.`, `..`, slash, backslash and NUL; `CleanRepoPath` uses it and refuses paths over 4,096 bytes; `checkTrees` applies it to every tree of pushes and Pulls. | `TestCleanRepoPath`, `TestValidPathComponent`, `TestApplyPackRefusesTreeBombs` (names) |
| 4 | Medium | **Zip export entries outside the folder.** Entry names were `path.Join(repo, path)`, which cleans `..`: a Commit holding `../evil.txt` (possible through a Pull before finding 3) became an entry one level above the Repo's folder, and a backslash name extracts as a path on Windows. | `internal/repo/jobs.go` | `dc3918b`: every entry must pass `CleanRepoPath` unchanged, or the Job fails naming the path. | `TestZipExportRefusesUnsafePaths` (fails without the fix) |
| 5 | Medium | **Unbounded pushes in count.** A push could send any number of ref updates (each a compare-and-swap in one transaction) and objects (each a record in go-git and `index-pack`), and push options were not bounded. | `internal/gitserver/receive.go` | `dc3918b`: 10,000 ref updates per push, 2,000,000 objects per pushed pack, unpacked size at most four times `git.max_pack_size` (at least 1 GiB). Malformed packs are refused rather than passed on, so a parser difference cannot skip the limits. | `TestPushRefusesTooManyRefUpdates`, `TestLimitReceivePackRefusesTooManyRefUpdates`, `TestPushRefusesMalformedPack`, `TestPackLimitsFor` |
| 6 | Medium | **SSRF through Upstream redirects.** The Upstream client followed any redirect to any address, including loopback, LAN services and the cloud metadata service at 169.254.169.254, and an Upstream URL of `0.0.0.0` reached the local host. A hostile or compromised Upstream (another Hangar, a mirror) could make Hangar send GETs into the local network; metadata answers could surface in error messages or Discover. | `internal/upstream/client.go` | `65f13b7`: network policy in [06](../../spec/06-upstreams-and-pulls.md) "Network policy". Reserved addresses are never dialled; a redirect off the Upstream's host may reach a private address only when that host is private itself; checks run in the dialer on the resolved address (so DNS rebinding cannot pass) with separate connection pools; refused requests are not retried. | `TestUpstreamRedirectsCannotReachTheLocalNetwork` (public Upstream refused, LAN Upstream allowed), `TestUpstreamNeverReachesReservedAddresses`, `TestCheckAddr` |
| 7 | Medium | **Webhooks to reserved addresses.** With `webhooks.allow_private_targets` on (the default), any Namespace admin, which includes every user for their own Namespace, could have Hangar POST to `0.0.0.0` (the local host) or the cloud metadata service. | `internal/webhook/service.go` | `65f13b7`: `httpx.IsReservedAddr` addresses are refused whatever the setting, with `ErrReservedTarget`, checked on the dialled address. | `TestReservedTargetsAreAlwaysRefused` (fails without the fix: the local receiver was reached through `0.0.0.0`) |
| 8 | Medium | **Symbolic links out of uploaded folders.** `hangar push`, `hangar import --from-dir`, `--from-lmstudio` and `--from-hf-cache` followed every link, so a cloned or downloaded model folder holding `config.json -> ~/.ssh/id_ed25519` uploaded the key, possibly to a public Repo. | `internal/cli/upload.go`, `internal/cli/hfcache.go`, `internal/upstream/importer/importer.go` | `9cf64d1`, `f7c80a7`: links are followed only into the folder, into the Repo's cache folder for a cache snapshot, or to a Hugging Face cache blob (as `hangar lmstudio` links); others are skipped and named on stderr. | `TestPushSkipsSymlinksOutOfTheFolder`, `TestSymlinksIntoACacheRepoAreFollowed`, `TestSymlinksToCacheBlobsAreFollowed`, `TestWithin`, `TestImportWithFakeTarget` (importer) |
| 9 | Low | **Unbounded Upstream ref advertisement.** An Upstream could advertise refs without end, each kept as a ref update of the Mirror. | `internal/upstream/gitfetch.go` | `8bf25b8`: `MaxUpstreamRefs` (100,000) for v0 advertisements and v2 `ls-refs`. | `TestFetchPackBoundsAdvertisedRefs` |
| 10 | Low | **kin-openapi CVEs.** GO-2026-6112 and GO-2026-6095 in v0.142.0; govulncheck reports neither as reachable. | `go.mod` | `97a9692`: v0.144.0. | govulncheck: no vulnerabilities in called code or imported packages |

### Phase 2 residuals verified

- **W6 cross-Namespace starvation (inputs item 3): fixed in Phase 2.** `requeueRefusedTransfer` queues the Pull's own transfer under `ownTransferKey` (`internal/upstream/plan.go`). With the key reverted to the bare OID, `TestPullQuotaOfAnotherNamespaceCannotStarveAPull` fails; with the fix it and `TestPullQuotaRefusalOfAJoinedTransferStandsWithinANamespace` pass.
- **W11 Xet index CPU amplification (inputs item 4): fixed in Phase 2.** Index loads are paid for by what a shard names (`Shard.indexReloadAllowance`, `internal/xet/indexcache.go`). With the allowance check disabled, two `TestShardIndexMemoryIsBounded` subtests fail (`reloads are paid for by the chunks a shard names`, `whole-xorb terms may reload as often as they name`); with it, `TestShardIndexMemoryIsBounded`, `TestIndexCache`, `TestIndexReloadAllowance`, `TestShardLimitsBoundTheWork` and `TestCASShardIngestsPerNamespace` pass.

### Checked and found sound

- Commit API and Hub API paths go through `CleanRepoPath` (absolute paths, `..`, NUL, backslash were already refused); `hangar download` checks every path before writing it.
- Pushes already ran `git index-pack --strict`, which refuses `..` and `.git` spellings; the new check adds backslashes and applies to Pulls.
- git-lfs batch requests are bounded (1,000 objects, 4 MiB body); upload-pack runs under `git.timeout` with a CPU limit on Linux.
- Webhooks do not follow redirects and already checked the dialled address for private targets.
- Upstream tree pagination stays on the Upstream's origin, and credentials are dropped on redirects to another scheme, host or port.

### Dependencies

- `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`: no vulnerabilities in called code or imported packages after `97a9692`. Module-level only: GO-2026-5932 (`golang.org/x/crypto/openpgp` is unmaintained; Hangar does not import it, and there is no fixed version).
- `pnpm audit --prod` in `ui/`: no known vulnerabilities.
- Python: nothing is shipped. `compat/uv.lock` (test suites) audited with `pip-audit` 2.9.0 over every pinned version, and `docs/requirements.txt` (docs site build): no known vulnerabilities.

## Residual risks

| Risk | Why it remains |
|---|---|
| Paths that differ only in case or Unicode normalisation (`Model.bin` and `model.bin`, NFC and NFD) can coexist in a Commit. A case-insensitive client keeps one of them on checkout. | Hangar treats paths as bytes, as git and the Hub do; refusing them would refuse Repos the Hub accepts. Both files are scanned and gated by path, so the scan gate still applies to each. Needs a decision from James if Hangar should refuse them. |
| Refs per Repo are bounded per push (10,000) and per Upstream advertisement (100,000), not in total. | Many pushes can grow a Repo's refs; the cost (ref advertisement size) falls on that Repo's clones. A total would need a new limit decision. |
| go-git's receive-pack has no wall-clock bound of its own, and nothing limits concurrent pushes per user. Each push can hold up to two objects of the per-delta limit (64 MiB by default) in memory. | Bounding concurrency needs a shared slot pool across replicas, as Xet's per-Namespace slots would. The per-object limits bound memory per push. |
| A proxy set with `HTTP_PROXY` or `HTTPS_PROXY` decides which addresses Upstream requests reach. | Hangar can only check the proxy's address; operators behind a proxy need it. Documented in 06 and the Mirrors page. |
| An Upstream's own host is trusted wherever it resolves (except reserved addresses), so an Upstream whose DNS an attacker controls can resolve into the LAN. | This is what lets admins use LAN Upstreams by host name; the admin chooses each Upstream. huggingface.co's DNS is trusted. |
| `webhooks.allow_private_targets` is on by default, so any Namespace admin (every user, for their own Namespace) can POST to LAN services. | Spec decision (10) for self-hosting; documented, with the advice to turn it off on shared Instances. Reserved addresses are now always refused. |
| IPv6 transition addresses that embed a private IPv4 address (6to4 `2002::/16`, Teredo) are not classed as private. | They route through relays on the internet, not to the local network; NAT64 (`64:ff9b::/96`) is classed private and its embedded address is checked for reserved ranges. |
| The Hugging Face cache blob exception lets a crafted folder link to a blob in the user's own cache, such as a file of a private model they downloaded, and upload it. | Needed so folders `hangar lmstudio` fills with links still upload. The link must name the blob's hash, so the attacker must already know the content's hash. |
| Pulls refuse whole Upstream Repos that hold a name a Repo cannot hold (backslash, `.git` spellings), or a non-LFS object rebuilt from a delta over 64 MiB. | Decided and recorded in [decisions](../decisions.md); such a file could not be served through the Hub API anyway. |
| The pack check unpacks every pushed pack once more (twice for pushes: before go-git and before `index-pack`), and `checkTrees` reads every distinct tree of each new Commit, including on server-side Commits. | The cost is linear in what the push or Commit holds. Large Repos (100,000 files) pay one `cat-file` process per Commit; the performance agent may want to measure it. |
| A Commit whose first parent is over the path limit gets every path's last Commit set to itself in the tree cache. | Only possible for Commits stored before the limit existed. |
| `internal/upstream/importer` is not used by the `hangar` binary (only by `compat/upstream`). | It follows the same symbolic link rule; whether to keep it is for the integration lead. |

## Test runs

At `bdd53d3`, on a host with a load average of 35 to 45 from other projects:

- `HANGAR_TEST_POSTGRES=1 make lint test` at `aa8a2ce`: golangci-lint 0 issues, Biome and `tsc` clean, Vitest 107 passed, every Go package passed on SQLite and Postgres with `-race` except two. `internal/repo` failed `TestGitProcessesPerOperation` ("create started 9 git processes", limit 7), caused by the new tree check; fixed in `2188682` and `bdd53d3`. `internal/upstream` hit the 30-minute package timeout while running `TestPullQuotaCountsObjectsTheInstanceAlreadyHolds` (load average 45).
- After the fixes, `HANGAR_TEST_POSTGRES=1 go test -race -count=1 -tags integration ./internal/repo/... ./internal/upstream/...`: all pass (`internal/repo` 224 s, `internal/upstream` 529 s, `importer` 245 s).
- `go test -count=1 -tags compat ./compat/git/ ./compat/lmstudio/`: pass (118 s, 11 s). `HANGAR_TEST_POSTGRES=1 ... go test -count=1 -tags compat ./compat/upstream/`: pass (341 s).
- `make check-generate`: no drift. govulncheck, `pnpm audit --prod` and `pip-audit` as in [Dependencies](#dependencies).

The Python compat suites (`compat/hf` with `huggingface_hub`) were not run: the changes reach them only through `CleanRepoPath`, whose newly refused names (`.git` spellings, paths over 4,096 bytes) no suite uses.

## Notes for the integration lead

- New packages and files: `internal/gitstore/packlimit.go` (with test helpers in `internal/gitstore/gitstoretest`, which has its own tests in `packs_test.go` so that `make cover` holds it to the 75% floor like the other helper packages), `internal/repo/treecheck.go`, `internal/httpx/netaddr.go`, `internal/upstream/netpolicy.go`, `internal/cli/symlinks.go`. `internal/gitserver` now imports `internal/gitstore` for the pack check.
- `repo.Options.MaxTreePaths` defaults to `repo.DefaultMaxTreePaths` (1,000,000); `cmd/hangar` needs no change.
- `upstream.NewHTTPClient` applies the network policy only when `HTTPOptions.Transport` is nil, which is how `cmd/hangar` builds it. A custom transport is used as given.
- The repo test helper `workdir.pack` now captures stdout only: `git pack-objects` writes warnings to stderr for paths it cannot check out, and those had been mixed into the pack bytes.
- While this work ran, the orchestrator's scratchpad folders (including `orchestrator-keep`) disappeared from the session scratchpad; this agent did not write to or remove them.

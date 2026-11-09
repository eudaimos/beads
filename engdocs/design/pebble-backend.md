# Pebble storage backend

> Design note for `internal/storage/pebble`, the pure-Go key-value backend
> behind the backend seam.
> Status: experimental. Phase 0 (prove the seam and the latency) is complete;
> the backend is linked into `bd` but is not the default and is not covered by
> the compatibility promises Dolt is.
> Date: 2026-11-08

## Why

Gas City drives beads as a library and as a CLI from many agents at once, and
measured its per-operation cost on Dolt: a small write (one no-history wisp
created at a pinned id, then closed through a fenced batch) cost tens of SQL
round trips, most of them the `issueops` write path maintaining its
invariants (the blocked-state fixpoint, the events row, the journal row) rather
than Dolt's commit graph. Cutting the no-op work on the SQL path
(gastownhall/beads#8225) helped but left the fixed per-op shape: config reads,
repeated row reads, hydration, events, each a round trip on any SQL backend.

The question this backend answers is whether the same invariants, computed in
process over an indexed graph, give a write that costs one atomic batch and a
few point reads. They do: on this host a fsynced create is about 5 ms and an
in-memory one about 250 µs, a ready page at 40k rows is about 1 ms, and a
limited list page is a few milliseconds, with every portable conformance case
and every issueops role contract passing. The numbers and the method are in
the package's latency tests (`latency_test.go`).

Beads is not becoming a storage engine (see
[PROJECT_CHARTER.md](../PROJECT_CHARTER.md)). Pebble is the engine; this
package is a second implementation of the storage interface, the way
`internal/httpclient` is, and it lives behind the same seam.

## What it is

Three layers, each its own change:

1. **The seam.** The backend registry (`internal/storage/backends`) gains an
   optional `Create` hook, and `bd init --backend=<name>` provisions a
   registered backend that supplies it. Nothing in this layer names Pebble.
   It is the init half of the pluggable-backend proposal
   (gastownhall/beads#4547): a registrant can already open a workspace
   through the registry; this lets it create one.
2. **The store.** `internal/storage/pebble` implements `storage.DoltStorage`
   over one Pebble database under `.beads/pebble/`: a JSON row per issue, the
   edges in both directions, events, comments, config, metadata, leases, the
   events journal, and the derived indexes that make the common reads bounded
   by the page rather than the store (ready candidates, blocked and deferred
   sets, row order, updated order per status, type, and the statistics
   counters). The blocked-state rule is the one the SQL union applies, run to
   a fixpoint over the affected neighborhood. Every issueops role accessor is
   served; version control, history, remotes, sync, federation and compaction
   answer `*storage.ErrUnsupported`. The key layout and the transaction model
   are in the package doc (`internal/storage/pebble/doc.go`).
3. **The linking.** `cmd/bd` links the package only in a build that carries
   the `pebblegozstd` tag (`cmd/bd/backend_pebble.go`), and `backend/pebble`
   is the public door for library embedders. The tag is Pebble's own switch
   for its pure-Go zstd. Pebble's default zstd is a cgo library that carries
   the same libzstd symbols Dolt's gozstd bundles, so a go-toolchain binary
   that links both must be built with it. Making Pebble part of every `bd`
   would thread that second tag through every workflow, release and install
   command, so the default `bd` is unchanged and the experimental backend is
   opt-in at build time. Under Bazel a module patch
   (`third_party/patches/pebble_pure_go_zstd.patch`) makes the pure-Go path
   unconditional, so the store's own tests run in the ordinary lanes with no
   tag.

## How it is proven

- `backend/conformance.RunAll`, the portable suite every backend runs.
- The role contracts: the package runs the conformance package's contract for
  every role it serves over a test-only responder that answers the contracts'
  scalar SQL reads from the key space (`rolekit_test.go`). The storage tree's
  contract-leg registry holds the leg to the whole role tier under an
  adoption ceiling (`internal/storage/contract_leg_pebble_test.go`); the
  ceiling counts the contracts that read history or need an id tombstone.
- Randomized scan-versus-index tests hold each derived index to its scan
  definition (`rowindex_test.go`, `readyindex_test.go`), including after a
  rebuild on open.
- Gas City's own `beadstest` suites run over the backend through its native
  store, and `bd` on a Pebble workspace matches embedded Dolt on a scripted
  create/ready/update/close/dep/list scenario.

## What it does not do

- **No history.** There is no commit graph: `bd history`, `--as-of`, `diff`,
  remotes, sync and federation are refused, and the commit family is a no-op
  so bd's auto-commit after each verb has nothing to fail.
- **No id tombstones.** A retaining sweep, the id-retention contract and the
  retired-id refusal of a pinned create cannot run until the store keeps them.
- **One process at a time.** A Pebble directory is held by one process. The
  deployment shape for many processes is `bd serve` on the Pebble store with
  every client on the `http` backend, which Gas City already treats as a
  self-sufficient library backend.
- **Not the default.** `bd init` still provisions Dolt, and a `bd` built
  without the `pebblegozstd` tag does not link Pebble at all. In a build that
  has it, Pebble is chosen by name and recorded in `metadata.json`.

## How it landed

The work was built as one spike branch and split into the three layers above,
each a branch on the eudaimos fork that stands on the one below:

| Layer | Branch | Base | Proposed where |
|---|---|---|---|
| 1, the seam | `pebble/01-backend-create-seam` | `main` | upstream (gastownhall/beads), as the init half of #4547 |
| 2, the store | `pebble/02-store` | layer 1 | the fork, until the scope conversation on #4547 settles |
| 3, the linking | `pebble/03-bd-link` | layer 2 | the fork, after layer 2 |

Each layer's PR body says what it contains, how it was proven and which lanes
ran; the layer 2 PR carries the live map of the stack and the follow-up list.
When the bottom merges, the next PR is retargeted to `main` and the rest
rebased. Gas City pins the fork's commit through a `replace` directive until
layer 3 reaches upstream `main`. gastownhall/beads#8225, the SQL-path cut
measured on the way here, is independent of the stack.

## Follow-ups

Each is a PR of its own on top of layer 3; the layer 2 PR tracks them.

- Typed read hooks for the role contracts, so the test-only SQL responder can
  go.
- Id tombstones, so the retaining sweep and the id-retention contract run.
- `GetCurrentCommit` for the export, backup and sync paths; export/import
  adoption; events-journal prune; `bd doctor` checks for a Pebble workspace.
- A same-transaction field-delta changelog to back `History`.

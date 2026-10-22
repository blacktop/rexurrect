# Rex compatibility

S0 covers capture of selected sessions from an isolated Rex server through
reconstruction into a freshly restarted server and comparison of observed state.
The installed CLI and its live API schemas are the authority for transport and
field semantics. A real server observation is the outcome oracle. This does not
establish persistence of commands, agents, terminal contents, or application UI.

Reference: Rex `0.0.1792088193-g7a93ec`, commit
`7a93ec7c8dde70d8eee38ab525392c2fa5b0b748` (Go 1.27.2, macOS arm64).

The table below was first committed with the S0 implementation; repository
history contains no pre-implementation closure matrix. The results are from the real
`TestIsolatedRoundTrip` run on 2026-10-15:

| Property | Required evidence | Status |
| --- | --- | --- |
| Explicit isolated endpoint | Private Unix socket, HOME/XDG directories, empty config, state directory, tracked server PID | Closed |
| Session selection and identity | Two explicit session IDs, duplicate display labels, 22 distinct old-to-new mappings | Closed |
| Windows and labels | Two ordered windows per session, labels observed again after reconstruction | Closed |
| Split tree | Horizontal 0.37 split containing a vertical 0.61 split; three shell leaves | Closed |
| Working directories | `process.child.cwd`, a symlink alias, and a shell cwd changed after creation | Closed |
| Active window, focus, zoom | Second window active; third nested leaf focused and zoomed | Closed |
| Native persistence baseline | Empty `session.list` after restart with the same state directory | Closed |
| Floating-layer detection | One created floating layer and its block ID explicitly reported as unsupported | Closed |
| Floating-layer reconstruction | No layer geometry, stacking, or block restoration | Unresolved |
| Detached-block detection | Reconcile `session.list_blocks` with every selected session's view; report the exact detached block ID, session, flavor, and reason | Closed |
| Detached-block reconstruction | No detached block restoration | Unresolved |
| Comparison oracle | Real round trip; mutation tests reject omissions and changes, including every missing ID mapping | Closed |

The normal-terminal recovery extension starts at cwd observation and ends at
comparison after a private-server restart. The retained live preflight at
`reboot-20261016T035446Z-23898/live-cwd-failure.json` is the authority for the
login/fish/OSC 7 response shapes; the installed Rex API and private server are
the creation and outcome authorities. Host reboot and installed LaunchAgent
acceptance remain downstream gates.

| Property | Required evidence | Status |
| --- | --- | --- |
| Cwd authority | `TestTerminalCWDLiveShapes` replays the live response shapes, proves fallback order, and rejects unusable paths with the block ID | Closed |
| Default terminal | The retained unsandboxed `TestDefaultTerminalRoundTrip` omits shell, command, and login options and observes login without cwd and fish with cwd | Closed |
| Restart recovery | The retained unsandboxed default-terminal test saves, restarts, restores, and compares under 25 journal ID mappings | Closed |
| Missing saved directory | `TestRestoreMissingCWDReported` verifies the recorded home substitution, lost-ack recovery, result reporting, and unchanged snapshot v1 | Closed |
| Automatic first run | The compiled auto binary publishes generation 1 from a fixture containing a default terminal | Unresolved |

## CLI and API surface

### CLI resolution

Each command resolves the Rex executable once, first using the configured
`--rex` executable when it is usable.
Otherwise it searches `/Applications/Rex Beta.app` and then
`/Applications/Rex.app`, using `Contents/Helpers/*/Contents/MacOS/rex` in
lexical order within each app, and finally searches PATH for `rex`. Only
executable files are accepted. This handles preview.15 moving the helper from
`Rex Server.app` to `Rex Server.rex` while `~/.local/bin/rex` still points at the
old path. Failure names the searched locations. Substitutions are printed to
stderr; automatic-cycle JSON also includes `rex_binary_substitution` with
`configured` and `resolved` paths. Every call in that command uses the resolved
path. If it disappears during the command, the call fails instead of silently
selecting another executable; the next command resolves it again. Resolution
neither starts Rex nor changes the explicit server endpoint.

`agent install` preserves an explicitly configured absolute Rex path in its
plist, even when it currently needs a fallback. A bare executable name is
resolved to an absolute path for launchd. `rexurrect version` (also
`--version`) prints the release version embedded by GoReleaser; source builds
report `dev`.

The root justfile defaults to `~/.local/bin`,
`unix://$HOME/Library/Application Support/rex/server.sock`, and the private
state directory `$HOME/Library/Application Support/rexurrect`. Its
`install` and `update` recipes rebuild and use the existing idempotent agent
installer; changed plist inputs reload only that job. `just --list` lists the
status, log, receipt, and LaunchAgent maintenance recipes. The recipes accept
the `REXURRECT_SERVER`, `REXURRECT_STATE_DIR`, `REXURRECT_REX`, and
`REXURRECT_BIN_DIR` environment overrides. Install the released binary with
`brew install --cask blacktop/tap/rexurrect`, or use `just install` to build
and install the per-user LaunchAgent. `just check` runs the formatting, vet, and
private-server checks. `just release-check` runs `goreleaser check`.
The release workflow pins GoReleaser v2.18.2 and publishes to
`blacktop/homebrew-tap` on branch `master`, under `Casks`, using the
`HOMEBREW_TAP_TOKEN` secret from the `release` GitHub environment.
`just release vMAJOR.MINOR.PATCH` requires a clean `main` checkout matching
`origin/main`, runs both checks with the native test gates enabled, creates a
signed tag at the checked commit, and pushes only that tag. If the push fails,
the signed local tag remains available for an explicit retry.

### Migration from rex-resurrect

Run this once from a rexurrect checkout in your macOS Fish terminal, before
installing the new agent. Use the old binary to remove its old LaunchAgent;
uninstall leaves snapshots, journals, receipts, and logs intact. The commands
below use the old defaults. Substitute any customized binary path, label, or
state directory; for a Homebrew installation, use `(command -s rex-resurrect)`
as the old binary path. If the destination already exists, stop and reconcile
the two state directories before continuing.

A late receipt clear or registration using the old state path can recreate the
old directory, leaving the new agent with a stale receipt or missing a new one.
Already-running wrappers retain their old paths, so finish their cleanup before
moving state and reload the updated hooks and wrappers before starting again.

1. Stop starting new agent sessions and other `rex-resurrect` commands.
2. Uninstall the old LaunchAgent with the old binary and label so its automatic
   job no longer writes. Continue only after this succeeds:

   ```fish
   "$HOME/.local/bin/rex-resurrect" agent uninstall --label com.blacktop.rex-resurrect
   ```

3. Let wrappers and hooks started earlier with the old binary or state path
   finish and clear their receipts. Let any other in-flight receipt, save,
   restore, or auto commands finish, then confirm that no `rex-resurrect`
   process remains. This must print nothing:

   ```fish
   pgrep -fl rex-resurrect
   ```

4. Only after those writers have drained, perform the guarded move. Continue
   only after it succeeds:

   ```fish
   test ! -e "$HOME/Library/Application Support/rexurrect"
   and mv "$HOME/Library/Application Support/rex-resurrect" "$HOME/Library/Application Support/rexurrect"
   ```

5. Update dotfiles hooks and wrappers to call `rexurrect`. Replace all
   `REX_RESURRECT_*` names with `REXURRECT_*`, including `SERVER`, `STATE_DIR`,
   `REX`, `BIN_DIR`, `SKIP_LAUNCHD`, and `SKIP_LOGIN_SHELL`; the old names are no
   longer read. Retain their values except for the moved state path. Set a
   custom `REXURRECT_STATE_DIR` to the moved directory before running
   `just install`. Rename `rex-resurrect-claude-hook` to `rexurrect-claude-hook`
   and update its Claude hook settings, then reload the updated hooks and
   wrappers.
6. Install the new agent:

   ```fish
   just install
   ```

7. After installation succeeds, resume launching sessions.

The snippets below use the new names. New operation markers start with
`rexurrect:`; recovery
continues to read the exact legacy `rex-resurrect:` markers in existing journals.

### Calls

The implementation uses only the installed CLI JSON interface. Every call has
an explicit `--server unix://PATH --autostart=false --timeout 20s` by default,
with a matching subprocess context deadline. `save`, `restore`, `status`,
`auto`, and `agent install` accept `--call-timeout DURATION`; it must be positive
and at most 60 seconds. The budget applies to each attempt. Failed `whoami
--json`, `server.version`, `session.list`, `session.view`, `session.list_blocks`,
`com.superlogical.terminal.pwd`, and `com.superlogical.terminal.process` calls
may be retried twice, with a
200-millisecond cancellable pause between attempts. Their caller's overall
deadline still applies; exhausted retries return an explicit error. Malformed
JSON and oversized output are not retried. Mutating and unrecognized methods
are never retried by the adapter. In particular, a timed-out `session.create`
retains the journal's uncertain-outcome and marker read-back behavior.

The larger default and bounded read retries accommodate login-time contention:
the former five-second CLI budget failed integration twice at host load averages
of 70–97, while the accepted LaunchAgent lifecycle passed twice on a quiet host.
This does not extend the automatic cycle's overall deadline or permit blind
mutation retries. Stdout and stderr remain limited to 4 MiB each.
Cancellation closes both read pipes, waits for their readers, and reaps
the CLI child even if descendants retain the write ends. API method names must
match `[a-z][a-z0-9._]*` before Rex is invoked. The integration server uses
`rex server run --listen unix://PATH
--state-dir DIR --config PATH`, a private environment, explicit resource limits,
and a tracked foreground process. Cleanup destroys sessions through that private
endpoint and signals only the process started by the test.

| API method | Use |
| --- | --- |
| `whoami --json` (CLI) | Read server host/instance identity; the sole automatic-cycle availability probe |
| `server.version` | Record the connected server's Rex version and commit |
| `session.list` | Readiness, initial/restart baselines, final cardinality, cleanup inventory |
| `session.create` | Atomic creation of ordered `initial_windows`; returned window and depth-first block IDs |
| `session.set_label` | Remove a restore operation marker after its result is journaled |
| `session.view` | Session/window labels, active window, layers, split tree, block inventory, focus and zoom; new layer/split IDs; structural revisions around save |
| `session.list_blocks` | Reconcile every registered block's identity, flavor, and placement with `session.view`; report detached blocks |
| `com.superlogical.terminal.pwd` | First cwd authority: the path of an OSC 7 file URL, ignoring its host |
| `com.superlogical.terminal.process` | Fallback cwd authorities: foreground process first, then child process |
| `session.focus_block` | Restore each window's focused block |
| `session.zoom_block` | Set zoom explicitly after focus |
| `session.focus_window` | Restore the final active window |
| `session.new_layer` | Create the unsupported floating-layer integration fixture |
| `session.new_block` | Create the unsupported detached-block integration fixture |
| `com.superlogical.terminal.write` | Change one fixture shell's cwd before capture |
| `session.destroy` | Tear down test-owned sessions during cleanup |

Discovery used `rex version`, `rex __internal skill`, CLI help, `rex api list`,
`rex api describe`, and `block_creator.list`. The inspected creation schemas also
included `session.new_window` and `session.new_split`; reconstruction uses
`session.create.initial_windows`. The installed server's schemas and observed
results take precedence over the general [Rex documentation](https://www.superlogical.com/rex/docs).

## Snapshot versions

Generations written by this version use snapshot version 4. Version 1
generations and journals written by `8974b2e`, version 2 generations written
by v0.1.0 (`58c87d7`), and version 3 generations written by v0.1.1 still load
and restore; the generation envelope, pointer, and journal binding formats
remain unchanged. Agent receipts and receipt-pruning reports belong to
snapshot v2; the host and command receipt form belongs to v3; encrypted
scrollback references and the `scrollback_skipped` report belong to v4.
Publishing upgrades an older snapshot in place without changing what any of
its receipts replay.

Downgrading cannot read newer generations and fails closed. The `8974b2e`
reader rejects every v2 shape; `TestSnapshotV1Compatibility` compiles that exact
reader and writer and checks `status`, `restore`, and `auto` preserve every
state file byte for byte. The v0.1.0 reader rejects v3 and v4 generations the same way: a v4 generation
with a scrollback reference fails on the unknown `scrollback` field, one
without on the version (`TestReleasedReaderCompatibility`). The `scrollback/`
directory and its pins are ignored by older readers' generation pruning. The transitional state is the exception:
a v0.1.0 generation beside a format 2 `receipts.json`, which exists as soon as a
hook registers after the upgrade and before the first v3 save. v0.1.0 `status`
still works on that state and `receipt list` fails on the unknown field, but
v0.1.0 `restore` creates the sessions and types their legacy commands before it
reads the store during receipt rebinding, then fails with a `created` journal
record and no `complete` (`TestReleasedRestoreTransitionalPrivateServer`).
Back up the state directory before upgrading; restore the backup, not the live
directory, if a downgrade is required.

Supported fields are the schema version; ordered sessions and windows with their
source IDs and display labels; active-window ID; tiled base-layer ID; recursive
split IDs, directions, ratios, and before/after children; shell-block IDs, labels,
and canonical cwds; and each window's focused and zoomed block IDs. `horizontal`
divides width, `vertical` divides height, and the ratio belongs to the before
(left/top) child. Labels never serve as lookup identities.

Capture first tries the terminal's `pwd` method. Its `pwd` field must be a
`file:` URL; only the decoded path is used, and the hostname is ignored. If
that path is unusable, capture tries `process.foreground.cwd`, then
`process.child.cwd`. Each candidate must be an absolute existing directory and
is canonicalized by resolving symlinks. If a pane has no usable cwd yet, capture
retries that pane twice with cancellable 200-millisecond pauses, within the
existing overall deadline and per-call budgets. If it still has no cwd, capture
stores the user's canonical home directory and records the block ID and chosen
cwd in `report.cwd_substitutions`. Manual save prints each substitution;
automatic summaries expose `capture_cwd_substitutions`. A failed process API
read or an expired deadline still fails explicitly. Restore verification requires
an observed cwd and never accepts this capture substitution as proof of recovery.
Only cwd-bearing fields are decoded from these responses;
command lines, environment, ownership metadata, and terminal contents are not
included in the snapshot.

Restore creates fresh terminals using Rex's defaults, passing the saved
cwd and, for an explicitly recorded agent, validated `initial_input`. It does
not override shell, command, or login.
`TestIsolatedRoundTrip` keeps its explicit `/bin/sh`, `login:false` source
fixture for determinism; `TestDefaultTerminalRoundTrip` additionally exercises
normal login terminals and checks every restored pane's process shape and cwd.
The explicit ID map
covers sessions, windows, tiled layers, splits, and blocks. It returns any
acknowledged partial mappings on failure and does not retry mutating calls.
Comparison requires an exact, one-to-one, kind-preserving map before comparing
the normalized snapshot. Snapshot validation bounds counts, tree depth, IDs,
labels, and cwd strings, and rejects dangling focus/zoom references.

## Save and status (S1a)

Build with `go build ./cmd/rexurrect`. Both commands require an explicit
absolute Unix socket endpoint and state directory; `--rex PATH` selects the
preferred Rex executable using the resolution order above. They never autostart a server. These
examples run in a macOS terminal:

```sh
./rexurrect save --server unix:///absolute/path/rex.sock --state-dir ./saved
./rexurrect status --server unix:///absolute/path/rex.sock --state-dir ./saved
./rexurrect status --server unix:///absolute/path/rex.sock --state-dir ./saved --json
./rexurrect save --server unix:///absolute/path/rex.sock --state-dir ./saved --allow-empty
```

Save inventories every live session. It reads `whoami --json`, `server.version`,
the session list, and every session's structural revision before and after the
existing Capture operation. A changed host, process instance, version, session
set, or revision discards the attempt. Three unstable attempts fail explicitly;
Rex reads have a two-minute overall deadline in addition to the per-call
limits above. The revision check covers the capture interval, not future
changes or independently changing process state such as a shell's cwd.

The state directory must have mode 0700; it is created with that mode when
absent, and newly created directory entries are synced in their parents.
Existing directories with other modes are rejected. All state files
must be regular files with mode 0600. The layout is:

```text
saved/
  lock                              # persistent inode, exclusive OS lock
  current.json                      # {"generation":2,"last_known_good":1}
  generation-00000000000000000001.json
  generation-00000000000000000002.json
  .tmp-*                            # ignored incomplete writes, if interrupted
```

Each generation is an envelope with `format_version: 1`, an increasing
`generation`, UTC RFC3339 `captured_at`, `rex_version`, `commit`, `host_id`,
`instance_id`, snapshot v2 in `snapshot`, and the capture's
unsupported report in `report`. The identity and build metadata describe the
connected server, rather than merely the local CLI binary. Status reports these
saved identities and counts supported sessions, windows, and blocks separately
from unsupported report entries. It checks that the supplied endpoint is
reachable and has the saved host ID; a new process instance after a restart is
allowed. `--json` emits one JSON object. Any failure exits nonzero.

Save and status hold the exclusive, nonblocking `lock` for their full state
operation; contention is an explicit error. Do not delete the lock file. Reads
are limited to 16 MiB per envelope and 1024 bytes for the current pointer.
Loading rejects malformed or trailing JSON, unknown fields or format versions,
invalid snapshot topology/references, missing or invalid unsupported reports, generation
mismatches, and missing or invalid last-known-good files.

Publication validates first, writes a private temporary file in the same
directory, fsyncs it, renames it, then fsyncs the directory. It publishes the
generation before replacing `current.json`; that single pointer update retains
the previous validated generation as last-known-good. After the pointer is
durable, older committed generation files are pruned under the same lock and
the directory is synced. This retains at most two committed snapshots, including
when upgrading a state directory with existing history. A pruning failure exits
nonzero and reports that the new generation was already published. Temporary
files are never candidates for current, and an
unreferenced final generation at the next number causes an explicit error
rather than being overwritten or adopted. Recovery of interrupted publication
is outside this slice. If a rename succeeded but its following directory fsync
fails, the command reports the durability failure; the rename may already be
visible.

Saving to a directory belonging to another Rex host is rejected. An empty
server cannot replace a nonempty current or last-known-good snapshot unless
the caller supplies `--allow-empty`. Guard failures leave existing generation
and pointer files unchanged. Save never destroys or mutates sessions.

`TestSaveStatusPrivateServer` uses the same private-server isolation as S0 and
the same save function as the CLI. It verifies two sessions, nested splits,
canonical cwd, focus, zoom, a floating layer, a detached block, envelope
round-tripping, last-known-good, status counts, and the empty-server guard.
Focused tests cover invalid and oversized state, leftover files, exclusive
locking, and transient/persistent structural revision changes.

## Journaled restore (S1b)

The restore CLI uses the same explicit endpoint, state directory, Rex executable,
and timeouts as save/status. It loads the current generation by default;
`--generation N` selects an older published generation. A valid current pointer
is required even when selecting a generation explicitly. `--json` prints the
generation, target host/instance, journal outcome, and supported source-to-live
ID map. A failure returns nonzero and retains acknowledged mappings in that
output and the journal.

```sh
./rexurrect restore --server unix:///absolute/path/rex.sock --state-dir ./saved
./rexurrect restore --server unix:///absolute/path/rex.sock --state-dir ./saved --generation 1 --json
```

The state directory also contains
`restore-NNNNNNNNNNNNNNNNNNNN-IDENTITY.json`, where `N` is the 20-digit generation
and `IDENTITY` is the SHA-256 hex digest of `host_id + NUL + instance_id`.
Each file has format version 1, the generation and target identities, and an
ordered `records` array. Each record repeats `generation`, `host_id`,
`instance_id`, `source_session_id`, `operation_marker`, and `outcome`, plus any
acknowledged `id_map`. The full next journal is written through the same private
temporary-file, file-fsync, rename, and directory-fsync path as snapshots. The
0700/0600 requirements, exclusive state lock, strict JSON validation, and
16 MiB limit apply. Restore leaves its selected generation file unchanged.

Before a new create, a saved directory that no longer exists is replaced with
the current user's canonical home directory. Other directory errors fail
explicitly. The create's durable `intent` records any substitutions in an
optional `cwd_substitutions` array, with `block_source_id`, `saved_cwd`, and
`restored_cwd`. Validation binds each entry to its original block and cwd and
rejects duplicates, invalid paths, or substitutions on later record types.
Recovery and comparison use this recorded destination even if the original
directory reappears. Restore/status JSON and the automatic-cycle result report
the substitutions; text restore output identifies each affected block. The
original snapshot is unchanged.

Before calling `session.create`, restore durably records `intent` with a fresh
`rexurrect:` marker followed by 32 random lowercase hex digits. That exact
marker is the creation-time session label. Display labels from the snapshot
never identify an existing session. An acknowledged create produces a durable
`created` record; a failed or timed-out call produces `uncertain`. Neither an
unresolved `intent` nor `uncertain` permits another create. On the next restore,
`session.list` and `session.view` read back the marker:

| Exact marker matches | Outcome |
| --- | --- |
| Zero | Explicit uncertainty error; no retry or mutation. A timed-out request may still be running. |
| One | Record `recovered` with that session ID, then verify its supported topology before configuration. |
| More than one | Explicit ambiguity error; leave the journal, sessions, and state directory intact. |

All outstanding creates are resolved before any new mutation. Restore fills the
map from `session.view`, preserving every prior acknowledged mapping, and checks
topology, labels, and canonical cwds through Capture and Compare. A durable
`configuring` record contains the full session/window/layer/split/block map before
`session.set_label` removes the marker and the focus, zoom, and active-window
setters run. Those setters can be repeated by recorded IDs after an interruption.
Only a successful final Capture/Compare records `complete` for that session.
Before resuming work, restore reads every recorded `complete` or `configuring`
session by its live ID with `session.view` and checks its supported topology
through Capture/Compare. A missing session produces an explicit
`recorded complete session ID is missing` error (`configuring` for an unfinished
record); a changed or unreadable session also fails verification. Completed
sessions are never recreated, and a failed read-back does not rewrite the journal
or repair later user edits.
The target host and instance are checked around creation and configuration;
a changed instance stops that attempt. A new server instance gets a separate
journal. Restore never destroys sessions or changes sessions outside its own
recorded creations.

Status reports `not_started`, `pending`, `uncertain`, or `complete` for the
current generation on the connected instance, with the recorded ID map. It also
checks the live session list for every completed record. If an otherwise
completed journal has missing sessions, the outcome is `complete_missing`.
JSON includes their recorded live IDs in `journal.missing_sessions`; text output
adds `Missing completed session ID` for each one. Pending or uncertain journals
retain their outcome and also report any missing completed sessions. A completed
journal remains a historical result: repeating that restore creates nothing and
succeeds only while every recorded session still matches the saved topology.
`TestRestoreRecoveryPrivateServer` exercises the CLI's restore function against
a private server, including duplicate saved labels, the complete supported S0
topology, unchanged unrelated-session views, completed-session disappearance,
idempotence, lost acknowledgements,
partial mappings, and ambiguous-marker recovery. Focused tests cover zero-match
uncertainty, missing current state, invalid/oversized journals, and generation
and instance binding.

## Automatic cycle (S2a)

`auto` runs one save/restore cycle. It requires the same explicit Unix socket,
private state directory, and optional `--rex PATH` as the manual commands. It
never starts a Rex server. Availability is established only by
`rex --server unix://PATH --autostart=false --timeout 20s whoami --json` by
default, using the read-only retry rule above and polling every two seconds
after a failed probe. `--wait` defaults to two minutes; `--timeout` defaults to five
minutes and covers the whole cycle, including the wait. Both durations must be
positive. The existing per-call limits still apply.

```sh
./rexurrect auto --server unix:///absolute/path/rex.sock --state-dir ./saved --json
./rexurrect auto --server unix:///absolute/path/rex.sock --state-dir ./saved --wait 30s --timeout 3m
```

The exclusive, nonblocking state lock is acquired before any Rex call and held
through publication. Manual save, restore, status, and another automatic cycle
cannot enter that state directory concurrently. Initial lock acquisition may
create the private directory and its persistent lock inode; unavailable or
blocked cycles do not publish generations or change existing state files.
The current envelope's `instance_id` is compared directly with live `whoami`.
A `not_started` journal alone does not mean Rex restarted.

| Decision | Condition | Action and outcome |
| --- | --- | --- |
| First-run | No current generation | Save generation 1; `first_run` |
| Same-instance | Saved and live instance IDs match; journal is `not_started` or `complete` | Capture without restoring; publish changes or return `same_instance` with reason `unchanged` |
| Same-instance-blocked | Same instance; journal is `pending`, `uncertain`, or `complete_missing` | Return `same_instance_blocked`; preserve current, last-known-good, and journal |
| New-instance | Same host, different instance; journal is `not_started`, `pending`, `uncertain`, or `complete` | Enter or resume restore, read back outstanding markers, verify recorded sessions, and create only missing sessions. Save a generation bound to the new instance only after restore reaches `complete`; `new_instance` |
| New-instance-unresolved | Different instance with `complete_missing`, or restore cannot resolve markers or finish verification/completion | Return `new_instance_unresolved`; preserve current and last-known-good. `complete_missing` and zero/ambiguous marker matches leave the journal and sessions unchanged; other interrupted restores retain their durable progress |
| Other-host | Saved and live host IDs differ | Return `other_host`; do not restore or save |
| Unreadable | Current, last-known-good, journal, or private state permissions fail validation | Return `unreadable`; do not save |
| Locked | Another process holds the state lock | Return `locked` before contacting Rex |
| Unavailable | `whoami` does not succeed before the wait or overall deadline | Return `unavailable`; do not restore or save |

The capture must still belong to the instance selected before recovery. A
restart during the cycle therefore fails instead of publishing an unchecked
instance. Automatic publication always uses the existing empty-server guard;
`auto` has no `--allow-empty` option. It never destroys sessions or retries a
mutation blindly. After a successful restore and save, the next same-instance
cycle captures user edits without replaying the historical restore. An automatic
capture identical to the current generation's server identity, snapshot, and
report ignores `captured_at` and publishes nothing. Receipt changes still pass
through durable publication and pruning. Manual `save` continues to publish a
new generation, even when the capture is unchanged.

After a Rex restart, wait for the triggered cycle (seconds) or run `auto` before
recreating sessions by hand. Restore never deduplicates user-made sessions.

Every cycle emits exactly one JSON object on stdout, including `state`,
`generation` (the selected current generation, or zero), `host_id`,
`instance_id`, and `restore_outcome`. A successful publication adds
`saved_generation`; an unchanged capture reports `reason: "unchanged"` without
`saved_generation` and exits zero. A failure adds its error as `reason`.
Human-readable diagnostics go to stderr. `--json` is accepted for explicit machine-oriented invocations; the
summary is JSON even without it. Other cycles that do not save exit nonzero.

`TestAutoCyclePrivateServer` exercises the same function as the CLI against a
private server, including real restarts, interrupted-creation adoption, resuming
a journal with one completed session, comparison under the journal ID map,
later window creation, byte-for-byte blocked-state preservation, unavailable
socket deadlines, and contention with a lock held by another process.
`TestAutoUnchangedPrivateServer` checks repeated unchanged cycles, supported and
unsupported changes, and manual-save publication. `TestSaveNewPaneWithoutCWDPrivateServer`
keeps a newly exited private pane open and verifies its home substitution in
the saved generation and unchanged automatic result.
`TestAutoBinaryPrivateServer` builds the CLI, creates explicit-shell and
default-terminal panes before its first run, checks publication of generation
1, checks same-instance JSON and exit status, and checks a nonzero unavailable
outcome.

## Opt-in agent receipts (S3a)

The receipt CLI uses the same explicit endpoint, private state directory,
non-autostarting Rex client, per-call deadline, and exclusive state lock as
save and restore. `set` requires a currently registered terminal block in the
named session. Nothing is inferred from process arguments, program status,
transcripts, or terminal contents. The harness selects the account wrapper;
credentials and shell environment remain the responsibility of that wrapper.

`receipts.json` is an atomically replaced, mode-0600 object containing
`format_version: 2`, the Rex `host_id`, and a `receipts` array. Format 1 files
are still read. The array has at most 1024 entries; the file is limited to
512 KiB and each entry to 1024 bytes. A registered entry has this shape:

```json
{"session_id":"session:01234567-89ab-4cde-8fab-0123456789ab","block_id":"block:01234567-89ab-4cde-8fab-0123456789ab","host":"codex","command":"codex-work","conversation":"01234567-89ab-4cde-8fab-0123456789ab","model":"gpt-6-astra","effort":"high"}
```

The key is the full Rex `session:UUID` and `block:UUID` pair. `host` is the
agent type, `codex` or `claude`, and selects the resume syntax. `command` is
the exact command name the launcher was started as and the word rexurrect
types; it matches `[A-Za-z_][A-Za-z0-9_.-]{0,63}`, so it is a bare word with
no spaces, metacharacters, or leading dash. Reserved shell words are excluded.
Conversation IDs must be UUIDs.
Optional models match `[a-z][a-z0-9]*([.-][a-z0-9]+)*` and have at most 128
bytes. Optional effort is `low`, `medium`, `high`, or `xhigh`. Registration
rejects unknown fields, duplicate keys, an unknown host, a missing host or
command, paths, whitespace, and shell metacharacters. No receipt contains
credentials, environment variables, executable paths, or cwd.

Entries written before format 2 carry a legacy `harness` instead of `host` and
`command`, and keep the execution semantics format 1 gave them: previously
resumable commands replay as before, while manual-only and unknown names remain
inert metadata. An entry that mixes a
harness with a host or command, or has only one of host and command, is
rejected. New integrations use `--host` and `--command`. The released
`receipt set --harness` form remains available for older hooks; its legacy
execution rules are unchanged. An explicit new-form registration replaces a
legacy entry for the same block.

Save copies a live block's receipt into its optional `agent_resume` field and
reports removed block IDs in `report.pruned_receipts`. It prunes the receipt
store only after publishing the snapshot; a rejected save leaves receipts
intact. Receipts for live unsupported layers or detached blocks remain stored
but are not replayed. Every present receipt is validated and must match its
enclosing session/block. A bounded, otherwise valid unknown legacy harness is
retained as snapshot metadata and restored as a plain terminal. Strict
state-file reads reject duplicate JSON object members at every depth,
including escaped spellings of the same name.

The [Rex terminal option](https://www.superlogical.com/rex/docs/reference/lua)
types the following text, followed by one newline, into the new normal terminal.
`UUID`, `M`, and `E` stand for validated values. Omit a model or effort flag when
that field was not recorded.

| Host | Exact typed command (`NAME` is the recorded command) |
| --- | --- |
| `claude` | `NAME --resume UUID --model M --effort E` |
| `codex` | `NAME resume UUID -m M -c model_reasoning_effort="E"` |

Reserved words in bash, zsh, and fish are rejected as command names. Legacy
entries retain their original command text; manual-only and unknown legacy
names get no input. This assumes the default
login shell defines the recorded command as a function or finds that
executable on its normal PATH. A receipt does not prove a login, model
entitlement, available conversation, or successful agent connection. Invalid
receipts fail restore before creation; the command builder also emits no input
for inert legacy entries, which receive a `not_resumed` entry (with the name
under `harness`) in the create intent, restore result, and automatic-cycle
result.

The write-ahead create intent covers the input along with the session creation.
Recovery adopts a creation by its operation marker; it never sends that input a
second time. Before marking a session complete, restore copies its receipts to
the acknowledged Rex IDs so the automatic cycle's next save retains them. An
original receipt is retained if its source session is still live.
Topology comparison validates receipts but excludes them from the observable
Rex topology comparison. A completed restore still verifies the mapped session
and never types again.

These examples run in the user's Fish terminal. Set the explicit endpoint and
state directory to the same values used by the automatic job:

```sh
rr_server=unix:///absolute/path/rex.sock
rr_state="$HOME/Library/Application Support/rexurrect"
rexurrect receipt set --server "$rr_server" --state-dir "$rr_state" --session "$REX_SESSION" --block "$REX_BLOCK" --host codex --command codex --conversation 01234567-89ab-4cde-8fab-0123456789ab --model gpt-6-astra --effort high
rexurrect receipt list --server "$rr_server" --state-dir "$rr_state" --json
rexurrect receipt clear --server "$rr_server" --state-dir "$rr_state" --session "$REX_SESSION" --block "$REX_BLOCK"
```

`list` optionally filters by `--session` and/or `--block`. `clear` optionally
takes `--conversation UUID` and `--command NAME` as guards; under the lock it
removes only a receipt matching every supplied guard, so a late SessionEnd hook
cannot clear a newer receipt in the same pane, and a launcher's exit cleanup
cannot clear a receipt another command registered. With both guards, both must
match. The command guard uses the same name grammar as `set` and also matches
a legacy entry whose harness is that name. The released `--harness` guard
remains an alias for older wrappers. Supplying both command-name flags with
different values is an error. A guard that matches nothing is a successful no-op.
Receipt commands wait up to 30 seconds for the state lock and return an explicit
timeout error if it stays held. `set` and `clear` accept `--lock-timeout
DURATION` (positive, at most 60 seconds) to shorten that wait; a hook whose host
enforces a hard deadline, such as a Codex SessionEnd hook limited to 3 seconds,
should pass a shorter value so a contended lock produces a reported failure
instead of a killed process. The default stays 30 seconds. Save, restore, and
automatic cycles retain their nonblocking lock behavior. Clearing affects future
saves, not an already published generation.

### Hook and wrapper examples

The [agent plugin](../plugins/rexurrect/README.md) is the packaged setup
for Codex and Claude Code. It keeps its scripts and skills together and provides
repository marketplace metadata. The examples below are the manually managed
alternative. When migrating, remove only the old rexurrect hook entries after
the plugin is installed and trusted; retain Rex's own session-start hook and
other unrelated hooks.

These are documentation examples only; the tool does not install hooks or edit
dotfiles. The [Claude Code hook reference](https://code.claude.com/docs/en/hooks)
defines the JSON input on stdin. The manual setup uses the plugin's
[receipt hook](../plugins/rexurrect/scripts/receipt-hook.sh) with the `claude`
host argument; copy it to a fixed path such as
`~/.agents/hooks/rexurrect-claude.sh`, start Claude through the plugin's
[`rexurrect-launch`](../plugins/rexurrect/shell/rexurrect-launch), and merge
these entries into the Claude Code settings. SessionStart's model takes
priority over the launcher's recorded default when supplied.

```json
{"hooks":{"SessionStart":[{"matcher":"startup|resume|clear|compact|fork","hooks":[{"type":"command","command":"sh \"$HOME/.agents/hooks/rexurrect-claude.sh\" claude","timeout":60}]}],"SessionEnd":[{"hooks":[{"type":"command","command":"sh \"$HOME/.agents/hooks/rexurrect-claude.sh\" claude","timeout":3}]}]}}
```

#### Codex lifecycle hook

Fresh Codex conversations need lifecycle hooks as well as the account wrappers.
Interactive Codex must run with `--no-daemon` inside Rex so its hooks receive
the owning pane's context and end when that foreground session exits. A TUI
attached to a shared daemon does not provide that lifecycle. This path was
verified with Codex CLI 0.163.0.
Upgrade rexurrect first and confirm that `receipt set --help` lists `--host`
and `--command`. Install `jq` if it is missing. The manual
setup uses the same [receipt hook](../plugins/rexurrect/scripts/receipt-hook.sh)
as the packaged plugin. From a stable checkout, copy it to a fixed path:

```sh
rr_plugin=/absolute/path/to/rexurrect/plugins/rexurrect
mkdir -p "$HOME/.agents/hooks"
cp -n "$rr_plugin/scripts/receipt-hook.sh" "$HOME/.agents/hooks/rexurrect-codex.sh"
```

If the destination already exists, review and update it before continuing.
Merge these two entries into the `hooks.json` of each Codex home you launch
from Rex. Both use the `codex` host argument; the launcher supplies the exact
harness through `RR_HARNESS` and `RR_HOST`. Preserve existing entries,
including Rex's own `rex __internal session-start --agent codex` hook. Do not
replace the whole file or copy trust hashes from another Codex home.

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "^(startup|resume|clear|fork)$",
        "hooks": [
          {
            "type": "command",
            "command": "sh \"$HOME/.agents/hooks/rexurrect-codex.sh\" codex",
            "timeout": 60
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh \"$HOME/.agents/hooks/rexurrect-codex.sh\" codex",
            "timeout": 3
          }
        ]
      }
    ]
  }
}
```

The [Codex hook payload](https://learn.chatgpt.com/docs/hooks) supplies the
conversation ID and model. The longer startup deadline leaves room for the
normal state-lock wait. The exit hook uses a 1500 ms lock wait and both receipt
guards, but its three-second host deadline can still interrupt a slow Rex call.
The launcher below provides a second cleanup attempt.
The hook records the model supplied by Codex and leaves effort empty because
the payload does not provide it; replay uses that account's effort default.
Compaction keeps the existing receipt. Nested agent invocations and invocations
without the launcher's ownership markers do not register or clear receipts.

#### Agent launcher

The plugin's [`rexurrect-launch`](../plugins/rexurrect/shell/rexurrect-launch)
is a POSIX `sh` script; install it on PATH and route `codex` and `claude`
through it with the provided [`rexurrect.sh`](../plugins/rexurrect/shell/rexurrect.sh)
functions for bash and zsh or the [`codex.fish`](../plugins/rexurrect/shell/codex.fish)
and [`claude.fish`](../plugins/rexurrect/shell/claude.fish) functions for fish.
The function name is the command rexurrect replays. The ownership markers,
`--no-daemon` rules, model and effort capture, exit cleanup, and limits are
described in the plugin README's
[ownership section](../plugins/rexurrect/README.md#give-the-root-agent-explicit-ownership);
this document does not repeat them.

Start `codex --no-daemon` and use `/hooks` to review and trust the two new
definitions; a daemon-attached TUI does not list newly installed hooks. Then
open a fresh conversation through the launcher and check
`rexurrect receipt list` with your configured endpoint and state directory.
Each entry must have the correct Rex block, harness, and conversation UUID.
Exit normally and verify that the matching receipt disappears. If either
operation fails, resolve that failure before relying on automatic resume.
Use a user-initiated Rex restart for the final recovery check, and verify the
actual resumed conversation identities; a completed layout journal alone is
not proof that Codex resumed successfully.
Codex 0.163.0 refuses to reopen a thread still held by the shared daemon and
asks you to close it in the other app. Quitting only the attached TUI can leave
the thread open; release it deliberately before relying on foreground recovery.

`TestAgentResumeRoundTrip` uses only a private Rex server and private login
shell functions, with no agent service connection. The allowlisted functions
register on start and clear their matching receipt on return. It checks the exact argv
and cwd after restart through `AutoCycle`, manual-only and crafted unknown
harness in the durable journal, receipt rebinding and clearing, mapped topology,
and an inert second restore. Its
real login-shell gate must run outside the agent sandbox:
`go test ./internal/rex -run TestAgentResumeRoundTrip -count=1 -v`.

## Encrypted scrollback (S4)

`save --scrollback` and `auto --scrollback` (both off by default) capture each
shell block's earlier output and replay it above the restored prompt. Nothing
about layout, cwd, or receipts depends on it: every failure below is a
per-block skip recorded in `report.scrollback_skipped` (and
`capture_scrollback_skipped` in the automatic result), never a failed save.
A failed terminal read or sidecar write is a skip too (`scrollback read: …`,
`scrollback sidecar: …`); only the save's own cancellation stops a capture. An
empty capture provisions no key, and the publication guards (same host, empty
snapshot over nonempty history) run before any scrollback state is written.
`save` and `restore` print each skip (`Block …: scrollback not captured (…)`
and `… not replayed (…)`); `auto --json` and `restore --json` carry the lists.

Capture, per block: `terminal.modes` must report the primary screen, then
`terminal.format` with `format: "vt", unwrap: true` (no `extra_*` flags), then
`modes` again; a block on the alternate screen at either read is skipped.
Rex's `code=too_large` response (its 1 MiB cap) is a skip, parsed from the
CLI's trailing `(status=NNN code=xxx)` pair. The output is sanitized to the
replayable subset measured in the contract probe: printable UTF-8, CR, LF, and
`ESC [ … m`. Every other escape sequence, control byte, and C1 character is
dropped. Empty output, more than 1 MiB per block, and more than 32 MiB per
generation are skips. Capture is sequential.

Storage: the sanitized bytes are zlib-compressed and encrypted with AES-256-GCM
under a key derived (HKDF-SHA256, random 32-byte salt) from a per-state-store
master key; the 92-byte header (`RRSBLOB1`, key ID, object ID, salt, plaintext
length) and a binding of state ID, Rex host ID, and source block ID are
authenticated. The object ID is an HMAC over binding and plaintext with a
separate derived key, so an unchanged block reuses its sidecar (after
authenticating it) instead of sealing a new one, without publishing a plain
content hash. Sidecars are immutable files
`scrollback/<key_id>/<object_id>.rrsb` (0600 in 0700 directories, ciphertext
in the temporary file too). No plaintext scrollback is ever written to disk.

Key lifecycle: the master key lives in the login keychain under service
`io.blacktop.rexurrect.scrollback.<key_id>`, written through `security -i` on
stdin and read with `find-generic-password -w`, each call bounded by a 3 s
deadline; a locked keychain prompts and blocks, so a timeout is "key
unavailable", not an error. `scrollback/state.json` holds the nonsecret state
ID, key ID, and a `pending`/`ready` status. Provisioning writes and syncs
`pending` in `scrollback/` (the temporary file lives beside it), creates the
item, then writes `ready`; a `pending` record whose item exists is
finalized. A `ready` key missing from the keychain is reported
(`scrollback key missing from the keychain`) and never regenerated. A backup
of the state directory does not contain the key.

Retention: after each publication, sidecars not referenced by the current
generation, the last known good one, or an unexpired pin are deleted, along
with temporary files; an unchanged automatic cycle repeats the pruning, so a
prune that failed after a publication is retried. `scrollback/`,
`scrollback/<key_id>/`, and `scrollback/pins/` must each be a real private
directory (mode 0700); a symlink or a shared mode refuses the write or the
prune rather than following it. Restore pins the references it is about to
replay (`scrollback/pins/<generation>`, listing them) before its first create
intent;
pins expire after one hour and are not released earlier, because the restored
shells read their sidecars at startup, which no journal record observes.

Replay: before any session is created, restore loads the key and authenticates
every referenced sidecar; blocks that fail are restored as plain shells and
listed in the restore result's `scrollback_skipped`. If the pin cannot be
written, every replay is skipped (`scrollback pin: …`) and plain shells are
restored. A block that passes is
created with `shell: "none"`, `login: false`, and a command that prints the
decrypted bytes and then starts Rex's own login chain
(`login -flp USER /bin/bash --noprofile --norc -c 'name=$1; shift; exec -a "-$name" -- "$@"' rex-login SHELL PATH`,
with the shell read from the directory service), so startup files run once as
a login shell, the `Last login:` banner separates old output from new, and the
agent `initial_input` is typed unchanged. The decryptor is
`rexurrect scrollback replay --state-dir DIR --key-id ID --object-id ID --host ID --block ID`
with the state directory made absolute, because the command runs in the
block's restored cwd; it reads without the state lock (sidecars are immutable and pinned), writes
to the terminal only after the whole blob authenticates, and writes nothing on
any failure, so the shell starts either way. Replay is decided once per
restore and only for the sessions that invocation creates, so a lost
acknowledgement cannot replay a block twice and a resumed restore neither
re-pins nor reports sessions it already created. A restore deadline that
expires while the key is loading fails the restore instead of degrading to
plain shells, so no create intent is recorded under a cancelled context.

`TestScrollbackCaptureAndReplayPrivateServer` runs this against a private Rex
server with a file-backed keychain stand-in: capture with an alternate-screen
skip, sidecar reuse on an unchanged save, replay through the login chain with
the agent command typed once and the replayed text never executed, and the
missing-key degradation. `REXURRECT_SECURITY_TOOL` substitutes the security
tool for that test only.

## Per-user LaunchAgent (S2b)

`agent install` installs and enables one job in `gui/UID`, where UID is the
current user. Its default label is `io.blacktop.rexurrect`; `--label` selects
a different label. The only installed file is
`~/Library/LaunchAgents/LABEL.plist`. `--plist-dir` supplies an absolute directory
override for isolated tests. The executable, Rex binary, and state directory in
the plist are absolute paths; keep both executables at stable locations across
reboots. The state directory retains its existing 0700/0600 requirements.

The installer uses structured `ProgramArguments`, `RunAtLoad`, `WatchPaths`, and
`ProcessType=Background`. It creates private `auto.stdout.log` and
`auto.stderr.log` files under the state directory. An identical install leaves
the plist bytes and modification time unchanged and does not boot out a loaded
job. Every install clears any persistent disabled override for that label.
Changed inputs replace only the owned labeled plist and reload that job.
Installing over an older owned plist without `WatchPaths` also replaces and
reloads it.
An existing plist without the matching ownership marker is rejected.

The commands below run in the user's macOS Fish terminal. Replace the socket
with the explicit endpoint of the Rex server that is meant to be preserved:

```fish
set -l rr_bin "$HOME/.local/bin/rexurrect"
set -l rr_rex (command -s rex)
set -l rr_server unix:///absolute/path/rex.sock
set -l rr_state "$HOME/Library/Application Support/rexurrect"
mkdir -p "$HOME/.local/bin"
go build -o "$rr_bin" ./cmd/rexurrect
"$rr_bin" agent install --server "$rr_server" --state-dir "$rr_state" --rex "$rr_rex" --interval 1m --wait 2m --timeout 5m
"$rr_bin" agent status --json
"$rr_bin" agent disable
"$rr_bin" agent enable
"$rr_bin" agent uninstall
```

All five subcommands accept `--server`, `--state-dir`, `--rex`, `--label`,
`--interval`, `--wait`, `--timeout`, `--call-timeout`, `--log-max-bytes`,
`--plist-dir`, and `--json`.
Install consumes the cycle settings; the other commands operate on the existing
plist selected by label and directory. The installed `ProgramArguments` retain
the configured per-call budget. Status reports `installed`, `loaded`,
and the exact domain and plist path. It does not infer persistent enablement
from launchctl's human-readable status text. Failures exit nonzero.

Enable and disable toggle a persistent per-label launchd override entry.
Disable marks that entry disabled and boots out the selected job, so no more
scheduled cycles run. Enable marks it enabled and bootstraps the existing plist
if needed; `RunAtLoad` starts a cycle. Uninstall marks the override enabled,
boots out the job, and removes only its owned plist. An enabled override entry
may remain in launchd; uninstall does not leave the label marked disabled.
Snapshots, generations, journals, logs, and the state directory remain intact.
Lifecycle operations use only `launchctl
bootstrap`, `bootout`, `enable`, `disable`, and `print` in `gui/UID`; they never
start a Rex server or operate on other labels.

The default interval is one minute. `--interval` accepts whole seconds from
one minute through 24 hours. `WatchPaths` contains exactly the configured Rex
socket path, so socket creation or removal can start a cycle within seconds.
The socket-removal run may start while Rex is down; it waits for the new socket
using the existing availability probe before deciding whether to restore.
The timer remains a safety net for missed filesystem notifications.
Apple documents the [filesystem and timer triggers](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html).
Login-time Rex availability uses the existing
two-second `whoami` polling; the default wait is two minutes and the whole cycle
has a five-minute deadline. Agent installation requires
`0 < wait <= timeout <= 1h` and `0 < call-timeout <= 60s`. If Rex is not available
yet, that cycle exits with an `unavailable` result; a later scheduled cycle can
try again. launchd runs
only one instance of a label, and the existing nonblocking state lock prevents
overlapping cycles and manual state operations, including across different
labels pointed at the same directory. An interval missed while the job is
running or the machine is asleep is not a queued replay.

Each automatic log defaults to a 1 MiB cap. `--log-max-bytes` accepts 1024
through 67108864 bytes. Each write opens the expected log inode in append mode
and holds an exclusive file lock through its size check and write. Lock
contention is bounded to five seconds. Before a write would exceed the cap,
`auto` truncates that log and writes `rexurrect: log truncated` as a marker
line. A single oversized message retains only its tail after the marker. Terminal output,
pipes, and files other than the two configured logs retain the normal output
format. Truncation affects logs only; snapshot and journal retention is unchanged.

### Reboot preflight and verification

Do this deliberately before a user-initiated reboot; neither the tool nor its
tests reboot or log out the host. No reboot was performed by the tool. Each
Fish block below is self-contained; replace its socket placeholder with the
same explicit endpoint chosen during installation.

1. Confirm the installed executable and Rex path will exist after login, the
   intended Rex server starts through its own normal mechanism, and the same
   explicit socket and private state directory will be available.
2. Install or enable the agent and verify its registration:

   ```fish
   set -l rr_bin "$HOME/.local/bin/rexurrect"
   set -l rr_rex (command -s rex)
   set -l rr_server unix:///absolute/path/rex.sock
   set -l rr_state "$HOME/Library/Application Support/rexurrect"
   "$rr_bin" agent install --server "$rr_server" --state-dir "$rr_state" --rex "$rr_rex" --interval 1m --wait 2m --timeout 5m
   "$rr_bin" agent status --json
   launchctl print gui/(id -u)/io.blacktop.rexurrect
   ```

3. Save once and retain the before-reboot status output. If the state lock is
   held by an automatic cycle, wait for that bounded cycle to finish first.
   Check the generation, host and instance IDs, session counts, unsupported
   entries, and journal outcome. Resolve any blocked or uncertain result before
   proceeding.

   ```fish
   set -l rr_bin "$HOME/.local/bin/rexurrect"
   set -l rr_rex (command -s rex)
   set -l rr_server unix:///absolute/path/rex.sock
   set -l rr_state "$HOME/Library/Application Support/rexurrect"
   "$rr_bin" auto --server "$rr_server" --state-dir "$rr_state" --rex "$rr_rex" --wait 2m --timeout 5m --json
   "$rr_bin" status --server "$rr_server" --state-dir "$rr_state" --rex "$rr_rex" --json
   ```

4. After reboot and GUI login, allow the normal Rex startup and bounded agent
   wait to complete, then run:

   ```fish
   set -l rr_bin "$HOME/.local/bin/rexurrect"
   set -l rr_rex (command -s rex)
   set -l rr_server unix:///absolute/path/rex.sock
   set -l rr_state "$HOME/Library/Application Support/rexurrect"
   "$rr_bin" agent status --json
   launchctl print gui/(id -u)/io.blacktop.rexurrect
   "$rr_bin" status --server "$rr_server" --state-dir "$rr_state" --rex "$rr_rex"
   "$rr_bin" status --server "$rr_server" --state-dir "$rr_state" --rex "$rr_rex" --json
   tail -n 5 "$rr_state/auto.stdout.log"
   tail -n 5 "$rr_state/auto.stderr.log"
   ```

   Confirm the host ID matches the retained status, the instance ID changed,
   and a newer generation was published. The successful `new_instance` cycle's
   JSON must report `restore_outcome:"complete"`. The corresponding
   `restore-GENERATION-IDENTITY.json` remains the durable historical journal;
   status for the newly saved generation can normally say `not_started`.
   Finally inspect the live Rex sessions and supported layout against the
   retained before state. Registration or a successful save alone is not a
   reboot reconstruction result. If the log shows `unavailable` or an unresolved
   restore outcome, retain the state and journal and investigate that result.

`TestLaunchAgentLifecycle` uses a compiled CLI, a private Rex server, a private
plist directory, and a unique label containing the test process ID. It checks
the exact `WatchPaths` socket, RunAtLoad and interval publication, unchanged reinstall that clears a direct
launchctl disabled override, a full disabled interval, enabled recovery after a
private-server restart, bounded logs, upgrade from a registered plist without
`WatchPaths`, and socket-removal activation within 15 seconds with a one-hour
timer. The triggered cycle must report `new_instance` with restore `complete`.
It also checks
byte-for-byte state/journal preservation on uninstall. It always cleans up its
own job and checks that uninstall does not leave its label disabled. This is a
live launchd gate, and bootstrap errors fail the test. An agent sandbox may
deny launchd submission even when the plist is valid. Inside such a sandbox,
run `env REXURRECT_SKIP_LAUNCHD=1 just check`; this explicitly skips only
the launchd lifecycle test. The real gate must still run outside an agent
sandbox, without that variable:
`go test ./internal/... -run TestLaunchAgentLifecycle -v`. A skipped or rejected
run does not establish lifecycle or reboot acceptance.

The macOS agent sandbox can also block the setuid `/usr/bin/login` path, causing
a default terminal to exit with status 127. `REXURRECT_SKIP_LOGIN_SHELL=1`
explicitly skips `TestDefaultTerminalRoundTrip`, `TestAgentResumeRoundTrip`, the older isolated/restore/auto
reconstruction tests (which now restore default terminals), and the
compiled-binary test's default-terminal fixture. All other tests still run.
The default-terminal test otherwise fails on a terminal exit; it does not
replace the default terminal with an explicit shell. A sandbox suite can use
`env REXURRECT_SKIP_LOGIN_SHELL=1 REXURRECT_SKIP_LAUNCHD=1 just check`.
Run the full real suite outside the sandbox without either variable. The two
focused default-terminal gates are:
`go test ./internal/rex -run TestDefaultTerminalRoundTrip -count=1 -v` and
`go test ./cmd/... -run TestAutoBinaryPrivateServer -count=1 -v`.

`just check` runs its full package suite with `go test -p 1 ./...` so the
packages do not compete with each other's private servers. The named integration
steps remain available. `TestSlowRexCalls` uses a stand-in executable to verify
six-second read responses, bounded retries, per-attempt cancellation, and the
single-invocation uncertain-create path without contacting a live Rex server.

## Unresolved features

Floating and other non-tiled layers retain their source session/window/layer IDs,
kind, block IDs, and reason in the capture report; they are not reconstructed.
Detached blocks retain their source session and block IDs, flavor, and reason in
individual unsupported entries; they are absent from the snapshot and restore
ID map. A mismatch between the registered block inventory and the session view
fails capture.
A non-shell tiled block, no usable absolute cwd, or focus/zoom pointing outside the
supported tiled tree fails capture. Save checks structural stability around
Capture; the underlying S0 Capture remains non-atomic. Remote hosts, alternate
block flavors, shell selection,
environment, pane ownership, client/app selection, layer bounds/stacking, and
terminal contents are not preserved. Other Rex versions and platforms remain
untested.

Arbitrary command replay and scrollback remain outside this slice. Agent
resumption is limited to the validated, opt-in receipt commands above.

# Tunnel-key watcher operation

The path unit watches only `data/machines.json`, never the SSH output directory.
The gateway atomically replaces that file. The synchronizer only reads it and
never writes the registry. Burst CRUD can otherwise exhaust systemd start limits.

The oneshot passes `--settle-seconds 3`: every invocation, including no-op and
invalid-input runs, waits before sampling. Changes during the wait coalesce into
one snapshot. A follow-up path activation handles changes during synchronization.
A 30-second/15-start safety limit remains enabled, above the healthy maximum of
10 starts per 30 seconds. Do not disable start limits to mask a spin loop.

Sampling requires identical bytes and file identity/stat across 200 ms, with a
2-second deadline. Busy, missing, malformed or structurally invalid input leaves
SSH output unchanged and fails with a redacted diagnostic. Invalid public-key
strings retain the existing behavior of omission, rather than authorization.
A valid empty registry revokes the managed block. Persistent failures retain the
last installed keys, so operators must investigate failed syncs promptly.

Output uses a private same-directory temporary file, file fsync, atomic rename
and directory fsync. A directory-fsync failure can happen after replacement;
errors do not imply that replacement was rolled back. No-op runs do not rewrite
output. Unmanaged keys and legacy adoption behavior remain unchanged.

For an existing failed installation, back up the script and units, install the
reviewed synchronizer/service changes, reload the user manager, reset failed state
for these two watcher units only, and start the path plus one explicit sync.
Never restart the gateway or workers for this repair. Verify a disposable disabled
machine CRUD burst, subsequent final-state convergence, bounded activations,
cleanup, unchanged real credentials, and a stable quiet observation window.

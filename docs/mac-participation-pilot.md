# Mac participation candidate, not deployed

Imported from the local Mac participation handoff (source commits
`e2e36182bdfb847ca8ae47b3da5c32a932ad3242` and
`e17d14f9175aa96cab6ae557eac4a5e30175bca6`, based on `11b7a9d`).
Ported onto `72e2a1b`, preserving benchmark admission checks and polling.

## User controls
Only Pause 1 hour / Pause 2 hours, plus Resume. Snooze persists locally; expiry
requires a new warning in the serving window. No pause-tonight option. Failure to
contact the service is an unknown state, not a claimed successful pause. Pause is
retried without renewing UI permission until confirmed.

**Unattended behavior:** only prior explicit `--enabled --automatic-participation`
enrollment permits starting after an ignored warning. The authenticated companion
must have authorized notifications, report successful warning submission and keep
sending fresh capable heartbeats. The warning grace must expire and idle/power/
thermal/resource checks must still pass. Submission is not proof of visibility
under Focus or consent; consent comes from enrollment, not silence. Missing/denied
notification authorization or failed submission holds processing. No nightly
confirmation click is required. Snooze expiry creates a new warning.
Default opt-out; AC only; configurable 600-second idle baseline; fresh normal
memory pressure required. Optional memory budget is a start-headroom check, reserve
is also checked while running. 6.6 GB is not a limit. No unqualified fixed allowance
is selected. Reclaim allowlist defaults empty; nonempty lists are deliberately
rejected pending named app/state-safe closure approval. No app killing is present.

Worker AC readiness also supports desktop Macs: the existing sensor reports
battery percentage zero when no battery percentage is present. AC no longer
requires a fictitious 5% battery; battery-powered workers retain the >20% floor.
Activity, thermal, benchmark admission and configured participation checks still
apply. A missing/unavailable participation service denies readiness even if the
gateway says to serve. Participation itself remains AC-only.

## Build and verify
`bash macos/NocturneCompanion/build.sh /tmp/nocturne-companion-build`
builds an ad-hoc-signed app. This is NOT Developer ID signing/notarization or proof
of notification delivery. Do not launch it in somebody else's GUI session.

Python tests: `python -m unittest discover -s tests -p 'test_*participation.py'`
and `python -m unittest discover -s tests -p test_model_supervisor.py`.
Use the repository venv for httpx agent tests.

## Controlled pilot preparation
`macos/prepare-participation.py --help` generates inert plists and an environment
merge fragment. It never edits installed launchd jobs. Select a consenting target,
console UID, hidden worker UID/GID and measured model settings explicitly.

Before any privileged installation:
1. Snapshot current installed worker/model plists and executable hashes; inspect
   current user activity and model ownership. Do not touch the SSH tunnel.
2. Install reviewed Python/code under a root-owned, non-group/world-writable path
   whose Python dependencies and ancestors are also trusted. Existing worker-owned
   `/opt/nocturne` code MUST NOT be run as root. Root controls argv; model executable
   must have a root-owned non-symlink ancestor chain. Model drops to worker UID/GID
   and clears supplementary groups before executing. Model artifacts may be read
   by that worker, but no worker-writable module may enter root Python import path.
3. Use private root-owned state `/private/var/db/nocturne-participation` mode0700, dedicated
   root-owned IPC parent mode0711. Separate0600 sockets grant UI control vs hidden
   worker status-only, authenticated by kernel peer UID. Never expose secrets.
4. Stop the old `io.nocturne.llama` supervisor, verify its model process is gone,
   then enable the new supervisor. Never run both. Preserve rollback snapshots.
5. Merge worker environment fragment into existing worker plist, preserving all
   unrelated keys; ensure new modules are packaged. The old installer remains
   legacy-only and does not enable participation. No silent fallback when a
   configured participation socket is unavailable.
6. Install companion LaunchAgent into consenting user's GUI domain and enroll
   only after notification permissions and local controls are verified.
7. Verify real cross-UID sockets, denied unauthorized mutations, pause during a
   lease/stream, expiry, reboot/sleep/wake, clock rollback, Focus/denied permission,
   console switch, memory pressure and actual CPU/GPU/RAM release. Model lifecycle
   uses owned child termination, not Mac reboot. No production workload is needed.

Rollback: stop new companion/service, verify owned model reaped, restore only
changed worker/model configuration from fresh inspected backups. Restore old model
service only with explicit user intent; doing so removes the participation guard.
Keep snooze state and evidence. Do not delete user data or prune shared artifacts.

## Acceptance not yet claimed
No opted-in target was designated. No employee prompts, live service installation,
model load/unload, application closure, measured reclaimed RAM, screenshot/GUI
acceptance, Developer ID signing, or real sleep/wake qualification has occurred.

## Integration and publication
The source handoff and this checkout have unrelated histories. Only participation
files were imported; `worker/agent.py` was integrated rather than replaced.
Benchmark admission remains required before leasing, after a lease returns and
during execution; benchmark polling and its shutdown cleanup remain unchanged.

Source publication to the public `lucasfth/nocturne` main branch was explicitly
approved on 2026-10-22. Publication is not deployment or installed-service acceptance.
The source handoff supplied no signing proof.
Rebuild and verify the generated app locally; ad-hoc signature verification still
does not establish Developer ID signing, notarization or notification delivery.

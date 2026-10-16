# Portable Mac onboarding

The canonical owner script is [`worker/nocturne-mac-setup.sh`](../worker/nocturne-mac-setup.sh).
It was already versioned at repository initialization; private deployment copies
are not the source of truth. Download script, config example and this guide from
the **same reviewed commit** (or clone the repository). Do not use an unaudited
curl-to-shell command or copy a private fleet script containing real keys.

## 1. Decide the identities and access

Get the owner's informed consent and an operator-approved maintenance window.
Owner preparation requires macOS, system Bash 3.2+, native `plutil`, standard shell tools
and OpenSSH `ssh-keygen`. Owner onboarding has **no Python dependency** and does
not install dependencies. Full worker provisioning additionally
requires Apple Silicon arm64, sufficient model disk/RAM capacity, network access
to the pinned downloads, Apple's `/usr/bin/python3` (Command Line Tools), and a
local administrator. Reference workers use M5/16 GB and M5 Pro/24 GB; other
hardware/macOS releases need independent qualification.

Keep these keys separate:

| Key/value | Obtain from | Used for |
|---|---|---|
| Operator SSH **public** key(s) | Each authorized operator supplies their `.pub`, with fingerprint verified independently | Operator → consenting owner's Mac account |
| Mac SSH host public key | Mac owner checks `/etc/ssh/ssh_host_ed25519_key.pub` locally | Operator verifies this Mac's SSH server |
| Gateway SSH host public key | Gateway administrator supplies `/etc/ssh/ssh_host_ed25519_key.pub`, fingerprint checked via console/trusted channel | Worker verifies gateway server; not a login authorization key |
| Worker tunnel key | Generated locally by `bootstrap-remote.sh` | Mac → gateway SSH, restricted port forwarding only |
| Worker secret | Generated locally by bootstrap, unique per Mac | Agent → worker API; gateway receives SHA-256 digest only |

Never provide private keys to owner setup. A host key obtained solely through
`ssh-keyscan` is not authenticated: compare its fingerprint with the console or
another trusted channel before pinning. Never disable StrictHostKeyChecking.

## 2. Edit portable owner config, then check

Copy [`worker/mac-setup.example.json`](../worker/mac-setup.example.json) outside
the checkout into a private setup directory (mode 0700), e.g. `~/Nocturne Setup`.
Use your own deployment values and public key files. The example has placeholders
only and intentionally cannot pass without real, valid public keys.

* `worker_id`: stable lowercase ID, e.g. `mac-example`, matching registration.
* `gateway_host`: actual gateway DNS name or IPv4 address, no URL/user/port.
* `gateway_ssh_user`: gateway's forwarding-only SSH account, not the Mac owner.
* `gateway_host_public_key_file`: verified gateway host `.pub` path.
* `operator_public_key_files`: 1–16 authorized operator `.pub` paths.

Relative key paths resolve beside the JSON, **not** the shell's working directory.
Absolute paths and paths containing spaces work. Config is parsed as JSON, never
sourced as shell code. No secret fields are accepted. Edit config for changing
keys/addresses/identity; edit the versioned script only for behavior changes, then
review the diff and rerun tests. Do not put real config into git.

```sh
bash worker/nocturne-mac-setup.sh --config "$HOME/Nocturne Setup/setup.json" --check
bash worker/nocturne-mac-setup.sh --config "$HOME/Nocturne Setup/setup.json"
```

Run as the actual owner, **without sudo**. `--check` (alias `--dry-run`) validates
inputs, public key structure and destination safety; it makes no writes, network
calls, clipboard changes or privileged calls. It does not prove key authenticity
or test connectivity. The script adds/replaces only its marked block in the
owner's `~/.ssh/authorized_keys`, mode 0600; `.ssh` is created 0700. Existing
unmanaged keys/config stay intact. First changed run preserves an original
`authorized_keys.nocturne-before` snapshot, 0600. Rejects symlinks/hardlinks and
shared/writable destination ancestors. Atomic rename protects readers, but standard shell tools do not provide explicit
file/directory fsync; sudden power-loss durability is not guaranteed. Run one instance at a time. If owner HOME
has a symlinked/shared ancestor, correct the filesystem through an approved
procedure rather than bypassing validation.

Enable **System Settings → General → Sharing → Remote Login**, allow only the
intended owner. If restricted by management/TCC, ask the administrator; the script
does not bypass controls. Record the Mac's SSH host fingerprint locally:

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

From the operator's machine verify a candidate host key against that fingerprint
out of band, then add the verified line to their `known_hosts`. Use
`ssh -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -i /path/to/operator_key owner@mac.example`.
Do not copy the operator private key to the Mac. Keep a second access/recovery
path. Owner reports/keys are not sent anywhere automatically.

**Behavior change from the old helper:** no hidden `nocturne-admin`, blanket
passwordless sudo, automatic sleep changes, or disabled macOS updates. Remote
administration must use an explicitly approved existing administrator with normal
sudo authentication, or local attended administration. Overnight lid-closed use
and energy/update policy require separate consent and OS-specific review; never
put an awake closed Mac in a bag. The worker supervisor uses caffeinate only
while serving, but that is not a universal lid-closed guarantee.

## 3. Operator provisions the worker (separate privileged stage)

Owner prep is **not** a worker installer. Follow [installer security](installer-security.md)
and [benchmark setup](model-benchmark.md). Independently stage/review the complete
`worker/` package in a root-owned, non-writable path with protected parents, e.g.
`/Library/NocturneInstaller`. Do not sudo a script from `/tmp`, Downloads, an
owner/worker-writable checkout, or a legacy deployment directory. Never "repair"
a worker-writable package by chowning and then executing it as root. Trusted
package provisioning is an administrator responsibility outside this helper.

With an already trusted package, **on the target Mac**, explicit approval:

```sh
sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
  /bin/bash /Library/NocturneInstaller/bootstrap-remote.sh mac-example
```

Bootstrap is fresh-only (`/opt/nocturne` must not exist). It creates the non-login
worker account, downloads checksum-pinned uv/llama/model, creates a protected
runtime and per-worker credentials. Secret and tunnel private key are 0600 under
private worker state. Never pass worker secrets as CLI arguments, log/copy them,
or reuse a shared default. Record **only** `DIGEST` and `PUBKEY`; normal status
lines may also be printed. Register the same ID and these values via admin
**Machines → Add machine** or [admin API](admin-api.md). Choose owner and SSH
metadata explicitly, never derive them from a sample inventory.

Ensure restricted tunnel-key synchronization is active on the gateway: authorize
only the generated tunnel public key with forwarding to `127.0.0.1:8701`, no shell
or other ports. Keep worker/admin listeners private. Review the reference model,
fixed local port 18701, SSH port 22, Europe/Copenhagen supervisor window and flags
in `install-services.sh` before provisioning; these remain reference deployment
choices, not dynamic gateway settings. Host supports DNS/IPv4, not custom ports
or IPv6 literals. Configuration below must match the owner config. The gateway
SSH username is now **required**, not a hardcoded personal username.

```sh
sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
  NOCTURNE_COORDINATOR_HOST=gateway.example.invalid \
  NOCTURNE_COORDINATOR_USER=operator \
  NOCTURNE_COORDINATOR_HOSTKEY='ssh-ed25519 REPLACE_WITH_VERIFIED_PUBLIC_KEY' \
  NOCTURNE_BENCHMARK_CATALOG=/Library/NocturneInstaller/catalog.json \
  /bin/bash /Library/NocturneInstaller/install-services.sh
```

The placeholder above must be replaced, not executed verbatim. Supply an approved
benchmark catalog per [benchmark guide](model-benchmark.md); do not silently skip
the gate. Installer writes known_hosts for that exact host and enforces
`StrictHostKeyChecking=yes`. Only public deployment values are in this command;
worker credentials stay in files. Services are system launchd jobs
`io.nocturne.tunnel`, `io.nocturne.llama`, `io.nocturne.worker`. Use exact labels,
not wildcard activation. Existing unsafe/legacy installs fail closed; do not
rerun fresh bootstrap over them.

## 4. Verify, maintain and remove

After an approved installation check `sudo launchctl print system/io.nocturne.worker`
and the tunnel/llama labels separately, protected runtime permissions, tunnel
connectivity, and the registered worker's heartbeat in the admin view. Online is
not ready: model health, schedule, idle, thermal, battery and benchmark/apply
state can legitimately prevent readiness. Inspect local logs without publishing
credential-bearing content. Test real generation only in an approved window;
unit tests are not live installation proof.

Rerun owner prep to replace its keys idempotently. It preserves unmanaged entries
but does not revoke identical keys installed outside its block. For removal:

```sh
bash worker/nocturne-mac-setup.sh --config "$HOME/Nocturne Setup/setup.json" --undo
```

Undo requires valid config/key inputs and removes only the managed block. It does
not delete accounts, disable SSH, reset sleep/updates, or undo legacy setup.
Compare the private first-run snapshot manually if needed; never overwrite newer
unmanaged keys blindly. Restore any old system policy from recorded pre-change
values, not guessed defaults. Review manually any legacy `nocturne-admin` account
and sudoers entry under a separate approved procedure.

Disable/remove registry entry to disconnect/revoke access; pause a worker with
`sudo -u nocturne-worker /usr/bin/touch /opt/nocturne/data/disabled`. Attended uninstall from the trusted
package: `sudo /bin/bash /Library/NocturneInstaller/uninstall.sh`. This removes
only services/plists and retains model, state and user. Explicitly approved
secure data/account erasure is separate. Keep OS updates and recovery available.

## Admin UI and release compatibility

Owner setup and worker bootstrap are separate steps, not a one-click deployment.
Admin instructions should link to `worker/nocturne-mac-setup.sh`,
`worker/mac-setup.example.json` and this guide at the same release commit. Do not
embed private public-key inventory or offer a credential-filled generated script.
No setup download HTTP route is introduced by this change; a repository link or
reviewed release artifact works independently of backend deployment. Existing
admin DIGEST/PUBKEY registration remains compatible. Operator automation invoking
service installation must add `NOCTURNE_COORDINATOR_USER` explicitly. Old blanket
admin bootstrap commands and `/tmp` staging instructions must not be reused.

### Paste registration output in the admin UI

After the separate, approved `worker/bootstrap-remote.sh` installation, copy its
`DIGEST <worker-id> <64-hex-sha256>` and optional
`PUBKEY <same-worker-id> ssh-ed25519 <public-key-base64>` output. The owner-only
`nocturne-mac-setup.sh` prints status instructions, not these registration values.
Do not paste shell commands, private keys, worker secrets or installation config.

In **Add machine**, the **Paste setup output** box is immediately visible above
the Worker id field, without expanding any sections. Paste the output, select
**Parse setup output**, review the worker ID and credential validation summary,
then **Apply to empty or matching fields**. Existing differing entries are not
overwritten: clear only the fields you explicitly intend to replace. Applying
clears pasted text and preview; **Add machine / Save changes** is still a separate
explicit action. Parsing/applying makes no API call and provisions no Mac.

Only the canonical DIGEST/PUBKEY labels are interpreted. Duplicate labels,
conflicting IDs, malformed digests/ed25519 wire structures and input above 16 KiB
are rejected without echoing values in errors. Other status lines are ignored;
partial output leaves absent fields unchanged. Quotes and shell syntax are not
interpreted. Owner and SSH/display metadata remain manual: gateway host/user are
not the Mac's host/user and are never inferred from configuration or examples.
Pasted text and parsed credentials stay in component memory, not browser storage,
URLs or logs, and are cleared on close. No automatic clipboard read is used.

### Import an older owner system report

An older owner helper emitted `=== nocturne mac report v2026-10-13.2 ===`
through `=== end ===`. The current portable owner helper uses native macOS tools
and does not emit that diagnostic report. Do not rerun the old script: it performed
privileged account/sudo, sleep and update-policy changes that are no longer the
approved owner preparation flow.

Previously copied reports remain importable through the visible paste box.
Review and apply only these explicit mappings:

| Legacy report label | Machine form field |
|---|---|
| `owner_name` | Owner |
| `ssh_user` | macOS user |
| `hostname` | Host |
| `lan_ip` | LAN IPv4 address; trailing `(wifi: …)` is not imported |
| `model` | Hardware description |
| `admin_user` | Reported account name; parenthesized status is not imported |

A legacy report does not supply a worker ID, worker secret SHA256 digest or
worker tunnel public key. These are never guessed from owner name, hostname,
`ssh_hostkey` fingerprint, Wi-Fi address, gateway settings or commands. The UI
recognizes the report and explains this distinction while allowing useful
metadata-only prefill. Registration still requires its existing credentials;
importing facts alone must not prompt a blind privileged bootstrap rerun.
Legacy `admin_user` is a historical fact, not permission to recreate that account
or grant sudo. Reports are parsed locally, using the same preview/no-overwrite
and explicit save safeguards as registration output.

Mixed report/registration output and duplicate/missing end markers are rejected; import metadata and registration output separately.

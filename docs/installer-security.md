# Installer security

`worker/bootstrap-remote.sh` and `worker/install-services.sh` now require a
reviewed, root-owned package with root-owned, non-group/world-writable parents.
Include `install-security.sh` and all five worker/benchmark Python modules in
that package. Do not run scripts from a worker account's checkout, shared
staging directory, or `/tmp`. Package directories/modules must be readable by
the worker for the unprivileged benchmark gate. Invoke the installers with a
clean environment, for example:

```sh
sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
  /bin/bash /trusted/package/bootstrap-remote.sh example-worker
sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
  NOCTURNE_COORDINATOR_HOST=coordinator.example \
  NOCTURNE_COORDINATOR_USER=operator \
  NOCTURNE_COORDINATOR_HOSTKEY='ssh-ed25519 REPLACE_WITH_VERIFIED_KEY' \
  NOCTURNE_BENCHMARK_CATALOG=/trusted/catalog.json \
  /bin/bash /trusted/package/install-services.sh
```

These are operator deployment commands, **not test commands**. Independently
review and provision the trusted package; never promote an existing
worker-writable tree to trusted status by chowning it. The installer/helper
itself must already be trusted before execution. Apple's system
`/usr/bin/python3` is required for isolated metadata validation and plist
serialization; the installed environment is never used for privileged Python.

## Ownership and legacy installations

Bootstrap is fresh-only. Unsafe existing trees fail closed, including unsafe
parents, ACLs, and symlinked code/destination paths. There is no in-place legacy
ownership repair. Stop services and reprovision compromised/legacy installs
offline under a separate operator-approved procedure. Do not reuse their code,
environments, caches or privileged download scripts.

`/opt/nocturne`, code, binaries, managed Python, venv and models remain
root-owned. Only `data`, `logs` and `home` are worker-owned (worker caches belong
under home/state). uv is forced to use managed Python under `python`, never a
system/Homebrew interpreter. Its cache is ephemeral under the private staging
directory, removed on installer exit; package installation uses copy mode so
the environment does not depend on that cache. No persistent uv cache is created.
Existing legacy `.install-cache` trees are still checked, not reused or repaired.

Generated interpreter links may connect within `.venv` and from there into
`python`; managed Python links must stay within `python`. Versioned release
library links must stay within `bin/llama`. Validation checks every link and
all ancestors hop by hop for root ownership, non-group/world-writable modes
(on non-links), and absence of ACLs, with a maximum of 40 link hops per path.
Cycles, dangling links, boundary escapes, and intermediate worker-state redirects
are rejected even when the eventual target is protected. Relative parent links
are allowed only while remaining within the permitted boundary. Other code and
destination links are rejected. Fresh runtime/release trees are validated too.
Root never traverses existing worker state for recursive ownership repair.
Service installation accesses state as the worker account, not root.

Downloads use a unique private staging directory under the protected install
root. Model fetching is now synchronous: a successful SHA-256 verification
precedes publication of a root-owned, read-only model. No fetch script in state
is executed, and no fixed shared staging/log path is used.

## Benchmark and limits

The existing benchmark gate and explicit `NOCTURNE_SKIP_BENCHMARK=1` behavior
are unchanged. Modules run as `nocturne-worker` with an empty environment plus
explicit settings, before daemon/config mutation; reports remain in worker
state. Catalogs must be worker-readable. Reports are worker-controlled evidence,
not trusted privileged input. An explicit skip remains unbenchmarked.

This protects against a compromised worker modifying code later executed by
root, symlink writes through state, and shared-staging substitution. It does not
sandbox the worker from its own state or protect against root/admin compromise,
a malicious reviewed package, compromised upstream dependencies, or concurrent
privileged modification. Run only one installer at a time. Dependency supply
chain pinning beyond the existing download hashes is unchanged. An already
compromised machine needs independent recovery, not permission repair.

Tests are shell syntax/static checks and temporary-filesystem policy fixtures
with simulated ownership; they do not install dependencies, start services,
exercise launchd, or constitute live macOS deployment verification.

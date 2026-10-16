# Security and release gates

This project is not cleared for public release by passing its unit tests.
Independent re-review of the security fixes and the benchmark/setup lifecycle
remains required. No live deployment or installation is part of that evidence.

## Trust boundaries

Public, worker and admin listeners are separate loopback endpoints. The reverse
proxy/tunnel configuration is trusted to route only the intended listener and
to replace identity headers. Do not publish worker/admin listeners directly.
Bearer secrets remain required; syntax screening is not authentication. Request
bodies are bounded before JSON decoding, including chunked requests. Body-only
idle/total deadlines do not shorten authenticated lease polls or SSE responses.

Admin mutations reject cross-origin browser metadata and explicit non-JSON
media types, except separately authenticated signed webhooks. Headerless CLI
clients remain supported. Origin checks compare the supplied Host; they are not
an arbitrary host allowlist and do not authenticate a hostile direct client.
Preserve Host at the trusted proxy, use the same-origin admin UI and retain
bearer verification. No forwarded-host trust is introduced.

Machine registry persistence publishes memory only after durable replacement.
A failure after rename poisons the registry until restart: worker authentication,
leases and further mutations fail closed rather than accepting old secrets.
Operators must inspect durable state before recovery. Pre-rename failures retain
previous authentication state. Invoice admission reserves per-key capacity before
outbound network calls; cancellation/failure releases reservations.

Worker inference HTTP clients ignore environment proxies. Worker code, managed
Python, environments and models must not be service-user writable. See
[installer security](docs/installer-security.md) for fresh-only provisioning,
trusted-package requirements and deliberate rejection of unsafe legacy trees.

## Remaining deployment/build gates

* Independently re-review security, installer changes and benchmark orchestration.
* The audited Go 1.26.6 toolchain has reported standard-library advisories. Build
  and vulnerability-scan release binaries using a patched supported compiler
  (audit minimum: 1.26.9 or 1.27.2). No host toolchain update is performed here.
* Existing systemd writable paths cover data, not root-level `config.json`.
  Config PATCH must remain unavailable/fail closed in that confinement until a
  dedicated writable configuration-state directory is designed and verified.
  Never make the executable/application root writable to accommodate PATCH.
* Exercise launchd installation/recovery on an explicitly authorized disposable
  Mac. Static checks and simulated ownership fixtures are not deployment proof.
* Compromised worker hosts, malicious reviewed installer packages, root/admin
  compromise and upstream dependency compromise require separate controls.

No private disclosure channel or response-time guarantee is established here.
Do not place live secrets, credentials, customer data or exploit-ready private
infrastructure details in public issues or test fixtures.

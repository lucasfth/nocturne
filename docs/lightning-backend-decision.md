# Lightning backend decision (2026-10-10)

Recommendation: **ACINQ phoenixd v0.9.2**, self-custodial mainnet, loopback HTTP.
It combines a signed native release, automatic inbound liquidity and invoice REST/API events.
No root, Docker, full Bitcoin node, identity registration or provider API key is needed.

| Candidate | Runtime/API and liquidity | Fees, backup and trade-offs |
|---|---|---|
| phoenixd | Native x64; REST invoices/lookup, signed webhooks; automatic ACINQ liquidity | Liquidity/service + mining fees; 12-word seed; established ACINQ stack, single LSP dependency |
| Alby Hub | User-hosted self-custodial wallet; NWC/apps; node/LSP setup and more moving parts | Channel/provider fees and optional hosting subscription; wallet/channel backup; mature ecosystem but larger operational surface |
| LNbits + external source | Python server; invoice/read API; source must supply liquidity | Custody, fees, seed/recovery depend entirely on source; extra server adds no value over direct phoenixd |
| Breez SDK / Spark | Nodeless self-custodial SDK, invoices/events; bindings/sidecar integration required | API-key onboarding; variable Lightning/exit fees, seed/persistence; newer Spark trust/exit model and integration work |
| Hosted no-KYC candidate (Coinos) | Merchant hosted invoice/API offering; no host dependencies | No-KYC advertised by directories, not independently guaranteed; API docs inaccessible in this run; custody/policy risk, not selected |

Alby Hub is not the legacy custodial Alby wallet. NWC is a protocol, not a custody guarantee.
LNbits alone is accounting/API middleware, not an automatic-liquidity wallet.
Hosted candidates do not satisfy the verified self-custody requirement here.

## Release authenticity and NixOS compatibility
Official release: https://github.com/ACINQ/phoenixd/releases/tag/v0.9.2
Archive: phoenixd-0.9.2-linux-x64.zip
Archive SHA256: c8ca4d380114aa69f3ff40ec0f8e632718f1c1252a02018d1ea05c541996d952
phoenixd binary SHA256: b05c6a202eebdc0801b8f2ac0b1e9af2468bdd037b5e3f70145de58dd0616c62
SHA256SUMS.asc GPG signature verified; signing fingerprint:
6AA45A4C209A2D3064CF66BEE434ED292E85643A (official acinq.co key fetched over TLS).
Key authenticity rests on that official HTTPS source; GPG owner trust is not independently certified.
Installed under ~/.local/opt/phoenixd/0.9.2; signed archive retained unchanged.
Coordinator host (NixOS): x86_64, glibc 2.42; direct binary fails at NixOS stub-ld (not glibc version).
`nix-shell -p steam-run --run 'steam-run .../phoenixd --help'` needed libcrypt.so.1.
Resolved with user-scope `nix-build '<nixpkgs>' -A libxcrypt-legacy` and `-A steam-run`.
GC-root result links: ~/.local/opt/phoenixd/{libxcrypt-legacy,steam-run}.
Unit uses pinned steam-run store result and LD_LIBRARY_PATH to libxcrypt-legacy/lib.
No /etc/nixos, Tailscale, privileged services or unrelated units changed.

## Operational behavior and actual fee evidence
Mainnet is the default; unit explicitly selects --chain=mainnet.
Default auto-liquidity is **2,000,000 sat**, not a 1,000-sat channel.
Authenticated GET /estimateliquidityfees?amountSat=2000000 at install:
**serviceFeeSat=21000, miningFeeSat=411; total 21411 sat** (variable quote).
This matches 1% of 2m liquidity + 1,000 sat initial-channel service fee, plus mining.
Default max mining fee is 20,000 sat and max non-refundable fee credit is 50,000 sat.
A first ~1,000 sat payment may be entirely non-refundable fee credit, NOT open a channel.
Owner must fund enough to cover a fresh liquidity quote before sale of prepaid credits.
Gateway credits invoice face value when payment lookup isPaid=true; merchant bears wallet fees.
Maintain treasury/fee budget separately; liquidity bootstrap is not customer gateway credit.

Seed file/config/files are 0600; **directory is 0700** (0600 would prevent directory traversal).
Back up 12-word seed offline and securely preserve wallet DB/config plus SQLite backups.
Never run the same seed in phoenixd and Phoenix mobile simultaneously: force-close risk.
No payment or sweep was executed. Only a real 1-sat unpaid invoice was generated.

Adapter reads phoenix.conf at startup, uses limited invoice/read password where available,
never places either password in config.json; raw-body HMAC-SHA256 X-Phoenix-Signature
is checked, and payment hash/externalId/requestedSat are independently rechecked via API.
Only admin :8702 accepts the webhook; public :8700 still returns 404 for that route.
Existing 30-second poller reconciles pending invoices, transactionally crediting once.
Unknown/duplicate events do not credit; late payments retain existing manual reconciliation policy.
No pending old-backend invoices existed when switching. Fake remains test-selectable.

## Sources checked
- https://github.com/ACINQ/phoenixd/blob/v0.9.2/src/commonMain/kotlin/fr/acinq/phoenixd/Api.kt
- https://github.com/ACINQ/phoenixd/blob/v0.9.2/src/commonMain/kotlin/fr/acinq/phoenixd/Phoenixd.kt
- https://github.com/ACINQ/phoenixd/blob/v0.9.2/src/commonMain/kotlin/fr/acinq/phoenixd/json/JsonSerializers.kt
- https://guides.getalby.com/user-guide/alby-hub
- https://docs.lnbits.org/guide/funding-sources-table.html
- https://sdk-doc-spark.breez.technology/
- https://kycnot.me/service/coinos (hosted/no-KYC claim only; not verified provider policy)
Phoenix server website returned 403; exact tagged source and live fee API used instead.

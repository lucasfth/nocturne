// Package lightning implements the Python invoice-only backends without startup
// side effects. New requires an explicit Backend: fake is solely an explicitly
// configured test backend, never a default or fallback. Adapters depend on app's
// capability contracts; app must not import lightning (startup composes both).
//
// Startup must supply the SAME billing instance to New and app.New, map expiry
// and min/max invoice limits consistently, and start App.RunInvoiceMaintenance
// under its shutdown context. Admin stays on its separate listener, retaining
// loopback-only Phoenix webhooks. Never mount admin on the public handler. The
// serve command remains intentionally disabled until full startup is complete.
//
// Phoenix prefers limited-access credentials, verifies HMAC over raw bytes,
// ignores notification amounts, and rechecks stored hash/external ID/amount at
// the daemon before billing's atomic, idempotent credit. Expired invoices never
// credit, matching Python. Notifications serialize with each other; concurrent
// polling may also observe payment, but billing credits/logs only once.
//
// Security hardening over Python: explicit backend selection, regular 0600
// credentials AND LND TLS certificate, HTTPS URL validation without URL userinfo,
// bounded response/secret reads, verified TLS >=1.2, no environment proxies on ANY
// backend, no redirects forwarding invoice credentials, and sanitized errors.
// Phoenix permits limited-only configurations (Python's eager fallback lookup
// accidentally requires the full password even when a limited one exists).
// Malformed upstream JSON/types fail closed as backend-unavailable instead of
// exposing daemon content. Only Phoenix exposes Wallet, matching the source.
// No outbound payment/withdrawal API or credential is implemented.
package lightning

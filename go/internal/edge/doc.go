// HTTP foundation integration notes
//
// Phase 4b supplies public routes to NewPublic or NewServers. Edge performs no
// JSON parsing, API-key billing admission, operating-window decisions, or route
// dispatch. Preserve energy admission Retry-After unchanged downstream; Python
// perimeter 429 responses intentionally have no Retry-After. Security wrapping
// supports streaming via Flush and ResponseController.Unwrap.
//
// Phase 4c supplies worker routes/coordinator lifecycle independently. Phase 4d
// supplies admin routes; TailnetGuard is additional filtering, not authentication.
// AuthenticateDigest and Bearer implement the worker/admin credential primitives.
// A request admitted through TailnetGuard must still authenticate normally.
//
// NewServers constructs only loopback HTTP servers, with header/idle deadlines
// and bounded in-flight requests. It does not listen or run maintenance tasks.
// The CLI serve stub deliberately remains unimplemented. Integration owns startup,
// graceful shutdown, request_id= key/value application logs, and readiness.
//
// Deliberate net/http differences: concurrency counts active handlers, not idle
// TCP sockets (uvicorn counts both); OS listen backlogs are not set to 64/128.
// net/http rejects malformed framing before middleware, so such wire requests
// may receive its own non-JSON 400. Direct handler calls get JSON length errors.
// Body transport errors get JSON 400 invalid body; Python disconnects silently.
// Config zero values select defaults; use negative limits to deny admission.
// Deadline enforcement requires native net/http or a ResponseWriter exposing
// SetReadDeadline; custom in-process bodies must honor their own cancellation.
// This avoids spawning a potentially unbounded goroutine per hostile read.
//
// No outbound client is created here. Future outbound integrations must retain
// Proxy=nil as in the existing fx and energy clients. Full gateway completion
// still requires lightning, build/systemd/README integration, Python-Go shadow
// comparison, and independent review.
package edge

# Project conventions

This file documents conventions this codebase follows in practice. Keep it in
sync when the convention changes — don't let it drift into aspirational
documentation for code that no longer matches.

## Factories over shared reuse

Some objects are unsafe to reuse once "used up" — an `http.Server` after it
has been `Serve`'d and shut down, an `http.Client` after its `Transport` has
broken, a `yamux.Config` shared across sessions. Rather than constructing one
instance and reusing it, these get a `*Factory` type that mints a fresh
instance on demand:

- `TunnelDialerFactory` (tunnel.go) — fresh `http.Client` per dial attempt
- `LocalServerFactory` (local.go) — fresh `http.Server` per tunnel session
- `YamuxConfigFactory` (tunnel.go) — fresh `yamux.Config` per session

Add a doc comment on the type explaining *why* reuse is unsafe, not just that
a factory exists — see local.go's `LocalServerFactory` for the pattern.

## Dependency injection: manual constructor injection, no framework

There is no DI container or service locator. `main.go`'s `runLocal`/
`runRemote` build every dependency (TLS config, proxy, factories, backoff)
and pass them into constructors, e.g. `NewLocalClient(opts, proxy, dialers,
servers, yamuxConfigs, backoff)`. Constructors take every dependency as a
parameter rather than constructing any of them internally, so nothing is
patched onto a struct after the fact. `main.go` is the only place wiring
happens.

When adding a new component, follow this shape: give it a constructor that
takes its dependencies as arguments, and wire it up in `main.go` — don't
reach for a package-level singleton or build dependencies inside the
component itself.

## Casing: PascalCase marks "used elsewhere in the package," not "public API"

This is `package main`, so no identifier is actually part of an external
API — but exported (PascalCase) vs. unexported (camelCase) is still used
meaningfully:

- **Exported** for anything referenced from another file in the package —
  `LocalClient`, `SessionRegistry`, `RequireOU`, `WithVersionHeader`, etc.
- **Unexported** for helpers that are purely local plumbing within one file
  — `sleepCtx` (local.go), `acceptTunnel` (remote.go), `peerCert` and
  `newYamuxSession` (tlsconf.go, tunnel.go).

When adding a function or type, default to unexported; only export it once
another file actually needs to call it.

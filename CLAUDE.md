# Project conventions

This file documents conventions this codebase follows in practice. Keep it in
sync when the convention changes — don't let it drift into aspirational
documentation for code that no longer matches.

## Factories over shared reuse

Some objects are unsafe to reuse once "used up" — an `http.Server` after it
has been `Serve`'d and shut down, a `yamux.Config` shared across sessions.
Rather than constructing one instance and reusing it, these get a `Factory`
type — a named `func() T` closure — that mints a fresh instance on demand:

- `LocalServerFactory` (local.go) — fresh `http.Server` per tunnel session
- `YamuxConfigFactory` (tunnel.go) — fresh `yamux.Config` per session

Add a doc comment on the type explaining *why* reuse is unsafe, not just that
a factory exists — see local.go's `LocalServerFactory` for the pattern.

**Name the variable, field, or parameter holding a factory `xxxFactory`**,
echoing its type: `serverFactory LocalServerFactory`, `yamuxConfigFactory
YamuxConfigFactory`. At a call site you should be able to tell a factory from
the thing it mints without looking up the declaration — `serverFactory()`
reads as "make a server", a bare `servers()` does not. This applies to
`LocalClient`'s struct fields just as much as to locals.

**Not every `http.Client` needs this.** `NewTunnelDialer` (tunnel.go) builds
one plain `*http.Client`, shared for the process's whole lifetime — no
factory. The general risk (a client reused past a broken `Transport`) is
real, but check whether it actually applies before reaching for this
pattern: `TunnelFactory.DialTunnel`'s doc comment has the reasoning for why
this particular client is safe to share across every dial attempt.

`TunnelFactory` (tunnel.go) is a related but distinct shape: dialing and
accepting a tunnel session are different operations, not two ways of minting
the same `T`, so it's a struct with `DialTunnel`/`AcceptTunnel` methods rather
than a `func() T` closure. It exists to hold the `yamuxConfigFactory` both
methods need, so callers build one `TunnelFactory` instead of threading that
factory through each call separately.

## Dependency injection: manual constructor injection, no framework

There is no DI container or service locator. Wiring happens in `main.go` and
nowhere else, at two levels:

- `run()` parses the flags, loads the `Config`, and builds the collaborators
  each half would otherwise construct for itself — the logger, the reverse
  proxy, the backoff, the session registry.
- `runLocal`/`runRemote` take those as parameters and build only what is
  specific to their half: the TLS config, the factories, the handler, the
  server.

Constructors take every dependency as a parameter rather than constructing any
of them internally, so nothing is patched onto a struct after the fact. The
three cert paths travel together as a `CertPaths` (tlsconf.go) rather than as
three positional strings.

Tunables follow the same rule — a constructor never hardcodes a duration.
`NewBackoff(min, max)` and `NewYamuxConfigFactory(keepAliveInterval,
streamOpenTimeout)` take theirs from the `Config` that `run()` loaded
(config.go), so the values live in one place and `--config` can override them.

When adding a new component, follow this shape: give it a constructor that
takes its dependencies as arguments, and wire it up in `main.go` — don't
reach for a package-level singleton or build dependencies inside the
component itself.

## Casing: PascalCase marks "used elsewhere in the package," not "public API"

This is `package main`, so no identifier is actually part of an external
API — but exported (PascalCase) vs. unexported (camelCase) is still used
meaningfully:

- **Exported** for anything referenced from another file in the package —
  `LocalClient`, `SessionRegistry`, `VerifyPeerRole`, `WithVersionHeader`, etc.
- **Unexported** for helpers that are purely local plumbing within one file
  — `sleepCtx` (local.go), `acceptTunnel` (remote.go), `leafCertOf` and
  `loadPoolAndCert` (tlsconf.go).

When adding a function or type, default to unexported; only export it once
another file actually needs to call it.

**Constructors are the exception to that default.** Anything whose job is to
build and return a value is PascalCase wherever it's called from —
`NewYamuxSession` (tunnel.go) is exported even though only tunnel.go calls it,
because `New` should mark construction on sight and a lowercase constructor
hides it. Where a bare `New` would lose track of *where* the value comes from,
name the source instead and keep the casing: `DefaultConfig`, `LoadConfig`
(config.go).

A type used only as a parameter of an already-exported constructor follows
the constructor's casing too, even though nothing outside its file
references the type itself: `YamuxRole` (tunnel.go) is exported solely
because `NewYamuxSession` takes one — a lowercase `yamuxRole` next to an
exported `New` would read as an inconsistency, not a scoping signal.

## Naming: say what a function does, not which field it touches

Prefer the name a caller would use to describe the call. `VerifyPeerRole` says
why the check exists where `RequireOU` only named the certificate field it read;
`GetPeerSubjectCN` says exactly what comes back where `PeerName` was ambiguous
between the CN, the SAN, and the host. Reach for the mechanism in the name only
when the mechanism *is* the point.

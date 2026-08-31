# AGENTS.md — Agent operating notes for opencode-proxy

This file covers the code conventions, environment reality, the release
pipeline, and how agents should work in this repo. Keep it in sync when facts
change — don't let it drift into aspirational documentation.

## Environment (established 2026-08-27)

- The automation sandbox runs as user `opencode` (uid 999, HOME=/var/lib/opencode);
  the human is `connor` (uid 1000, /home/connor). These are DIFFERENT users: no
  shared `~/.config/gh`, `~/.ssh`, or shell state. There is no passwordless sudo.
- Assume non-interactive execution: browser/device auth flows cannot render into
  the human's terminal. Anything only the human can provide (tokens, decisions)
  must be requested in chat; credentials arrive via the token-file flow below.
- `gh` CLI v2.98.0 is installed user-local at `/var/lib/opencode/.local/bin/gh`.
  PATH is NOT persisted across shells — always run
  `export PATH="$HOME/.local/bin:$PATH"` first. Authenticated as `ChronicCmposer`
  (fine-grained PAT; confirm with `gh auth status`). Git-over-SSH uses the sandbox
  key `id_ed25519`, already registered on GitHub.
- Repo-local git identity must be set before committing:
  `git config user.name "ChronicCmposer"` and
  `git config user.email "265143892+ChronicCmposer@users.noreply.github.com"`
  (git identity was unset by default).
- buildkitd serves the OCI build socket at `/run/buildkit/buildkitd.sock`
  (the Makefile `BUILDKIT_HOST` default). The daemon may NOT be running — gate
  any build with `make check-buildkitd` first. `buildctl` is at
  `/usr/local/bin/buildctl`. Sandbox access to the socket was granted via
  setfacl ACLs (helper `grant-buildkit-access.sh`, gitignored).

## Release pipeline

`scripts/release.sh` fails fast unless ALL of these hold:

- working tree is empty: `git status --porcelain` prints nothing
- local `HEAD` == `origin/main`
- `HEAD` is exactly tagged: `git describe --tags --exact-match HEAD`
- that tag exists on origin
- `gh` is installed and authenticated
- `signing/release.key` exists
- buildkitd is reachable

Keep the tree clean: `plans/`, `/deploy-state.json`, `/delete-stack.sh`, `/step-*.sh` and sandbox strays (`1`, `out.txt`, `grant-buildkit-access.sh`, `gh_auth_login.py`) are gitignored. Do not let new
untracked files accumulate — a squash's `git add -A` sweeps them into the commit,
and `release.sh` refuses a dirty tree.

History convention: `main` carries a squash of the feature branch
`claude/opencode-aws-proxy-spec-ivsukr` (`git reset --soft <merge-base>` + one
commit, then a fast-forward merge). The origin feature branch is intentionally
left untouched; local copies are rewritten by the squash (old commits survive in
the reflog).

Pre-release workflow: `make release` has no prerelease flag — mark it after the
fact with `gh release edit <tag> --prerelease`.

Known caveat: the `/releases/latest/download/opencode-proxy.tar` URL (used by
`scripts/update-image.sh` and `cloudformation/stack.yaml` `ImageTarURL`)
returns 404 while the newest release is a prerelease — it resolves only after a
stable release is published (as of 2026-08-28 the latest stable is `v0.0.4`).

## Deployment & operations (established 2026-08-28)

- **Deployed state is authoritative in `deploy-state.json`** at the repo root
  (stack, domain, EIP, instance_id, repo_ref pin). Cross-check it before any
  deploy; never invent deployment constants from memory.
- **RepoRef pins commit SHAs, not tags.** Tags drift from main: the `v0.0.2`
  tag ships systemd units with the ctr-run bug (fix = `6ebd4d2`; the deployed
  stack and the local VM both pin that SHA). Verify what a tag actually
  contains (`git show <tag>:systemd/...`) before deploying from it, and prefer
  a reviewed commit SHA over a tag name.
- **Boot SUCCESS != healthy service.** CloudFormation's `CREATE_COMPLETE` and
  the boot signal only prove UserData exited 0. After any instance or VM
  deploy, verify the service is actually listening: `systemctl status <unit>`
  / `journalctl -u <unit>` / `ss -tlnp` / `ctr task ls`.
- **`ctr run` positional args replace the image ENTRYPOINT.** The binary must
  be named explicitly as the first arg after the container name:
  `ctr run ... ${IMAGE_REF} <name> /opencode-proxy --remote ...`. Both systemd
  units do this and carry a comment — preserve it in any edit.
- **CloudFormation mechanics:** `cloudformation/deploy.sh` passes every
  parameter explicitly (InstanceType always pinned; default t4g.micro). A
  template parameter Default change does NOT reach an existing stack. A
  UserData change is stop/start, NOT replacement; an InstanceType change is
  the reliable replacement trigger. Never terminate an instance out-of-band —
  CloudFormation can then never update that resource (delete + recreate only).
  `UPDATE_ROLLBACK_FAILED` recovery:
  `aws cloudformation continue-update-rollback --stack-name <stack> --resources-to-skip Instance`.
- **The TLS gate is strict mTLS.** A bare `curl` always fails the handshake by
  design. All probes must present a client cert (`pki/out/phone.crt` +
  `phone.key`); the 503 + `X-Opencode-Proxy-Remote-Version` response only
  appears after a valid client-cert handshake.

## Signing key custody

- `signing/release.key` — PRIVATE. chmod 600, gitignored (`/signing/`). Never
  commit, copy, or echo it; keep it offline. Regenerate via
  `scripts/init-signing-key.sh`.
- `scripts/release-signing.pub` — PUBLIC, committed. Hosts verify releases
  against it.
- To verify the keypair:
  `openssl pkey -in signing/release.key -pubout` must be byte-identical to
  `scripts/release-signing.pub`.

## Secrets handling

Token/credential values: write them to a temp file created with `umask 077`,
consume via stdin (e.g. `gh auth login --hostname github.com --git-protocol ssh
--with-token < file`), and delete the file immediately after. Never place token
values in argv, stdout, logs, or reports.

## Agent workflow conventions

- All repo mutations are performed by delegated coder subagents with detailed
  prompts; the orchestrator never edits files or runs commands directly.
- Subagent reports must include verbatim commands and their outputs — this makes
  verification trustworthy and auditable.
- Load the `code-philosophy` skill (and `frontend-philosophy` for UI work)
  before writing or modifying code, and verify against its checklist before
  completing.
- Keep a running state summary and compress closed phases to control context
  during long sessions.
- At deploy gates, require verbatim outputs from the human (`out.txt`), not
  status words — terse reports have hidden real failures. Bake exact commands
  into gitignored `step-*.sh` scripts rather than pasting commands into chat;
  copy-paste/history silently drops arguments.

## Code conventions

Conventions this codebase follows in practice. Keep them in sync when a
convention changes — don't let them drift into aspirational documentation for
code that no longer matches.

### Factories over shared reuse

Some objects are unsafe to reuse once "used up" — a `yamux.Config` shared
across sessions, for instance. Rather than constructing one instance and
reusing it, these get a `Factory` type — a named `func() T` closure — that
mints a fresh instance on demand. Add a doc comment on the type explaining
*why* reuse is unsafe, not just that a factory exists, and name the
variable/field/parameter holding one `xxxFactory`, echoing its type: at a
call site you should be able to tell a factory from the thing it mints
without looking up the declaration — `yamuxConfigFactory()` reads as "make a
yamux config", a bare `yamuxConfigs()` does not.

**Don't reach for this reflexively — check whether reuse is actually unsafe
first.** `TunnelDialerFactory`, `LocalServerFactory`, and
`YamuxConfigFactory` all used to exist here, minting a fresh
`http.Client`/`http.Server`/`yamux.Config` per use on the assumption that
reuse was unsafe. All three assumptions turned out to be wrong once actually
checked: `NewTunnelDialer` and `NewYamuxConfig` (tunnel.go) each build one
instance now, shared for the process's whole lifetime; the local
`*http.Server` is built the same way but inlined directly in `run()`
(main.go), with no constructor of its own — no Factory or dedicated
constructor for it currently exists in this codebase. The comment at each
construction site carries the reasoning for why that reuse is safe; don't
restore a factory — or a constructor for the local server — without redoing
that kind of check.

`TunnelFactory` (tunnel.go) is a related but distinct shape: dialing and
accepting a tunnel session are different operations, not two ways of minting
the same `T`, so it's a struct with `DialTunnel`/`AcceptTunnel` methods rather
than a `func() T` closure. It exists to hold the `yamuxConfig` both methods
need, so callers build one `TunnelFactory` instead of threading that value
through each call separately.

### Dependency injection: manual constructor injection, no framework

There is no DI container or service locator. All wiring happens in one
place: `run()` (main.go) parses the flags, loads the `Config`, and builds
every collaborator each half needs — the logger, the reverse proxy, the
backoff, the session registry, the TLS config, and whatever else differs
between local and remote — before branching on `f.isLocal` to run whichever
half's client/server loop.

Constructors take every dependency as a parameter rather than constructing any
of them internally, so nothing is patched onto a struct after the fact. The
three cert paths travel together as a `CertPaths` (tlsconf.go) rather than as
three positional strings.

Tunables follow the same rule — a constructor never hardcodes a duration.
`NewBackoff(min, max)` and `NewYamuxConfig(keepAliveInterval,
streamOpenTimeout)` take theirs from the `Config` that `run()` loaded
(config.go), so the values live in one place and `--config` can override them.

When adding a new component, follow this shape: give it a constructor that
takes its dependencies as arguments, and wire it up in `run()` — don't
reach for a package-level singleton or build dependencies inside the
component itself.

### Casing: PascalCase marks "used elsewhere in the package," not "public API"

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

### Naming: say what a function does, not which field it touches

Prefer the name a caller would use to describe the call. `VerifyPeerRole` says
why the check exists where `RequireOU` only named the certificate field it read;
`GetPeerSubjectCN` says exactly what comes back where `PeerName` was ambiguous
between the CN, the SAN, and the host. Reach for the mechanism in the name only
when the mechanism *is* the point.

## PKI & device profiles

- `pki/issue-client.sh <name>` produces `<name>.mobileconfig` — an Apple
  profile bundling CA trust (`com.apple.security.root`) and the device
  identity (`com.apple.security.pkcs12`) in one install. The intermediate
  `.p12` is shredded on exit (`trap ... EXIT`), so the `.mobileconfig` is the
  ONLY deliverable — do not tell users a standalone `.p12` exists.
- Validity windows: device certs 30 days, server/tunnel certs 90 days, the CA
  10 years. `renew.sh` tracks the 90/30-day leaves, not the CA.
- The embedded p12 is built with legacy-compatible algorithms (`PBE-SHA1-3DES`
  + SHA-1 MAC) because Apple's SecKeychainItemImport rejects some
  OpenSSL-default cipher suites on macOS ("MAC verification failed" /
  "certificate could not be verified") — this is why the same profile installs
  on both iOS and macOS.
- macOS install flow: the profile is UNSIGNED, so macOS 11+ only installs it
  via System Settings → General → VPN & Device Management → Downloaded →
  Install, accepting the "not signed" warning; then enable full trust for the
  opencode-proxy CA in Keychain Access (Always Trust). iOS installs the same
  unsigned profile via Settings → General → VPN & Device Management, then
  Certificate Trust Settings. Always delete the `.mobileconfig` after install
  — it carries the device key + p12 password in plaintext.

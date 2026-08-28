# AGENTS.md — Agent operating notes for opencode-proxy

Code conventions live in `CLAUDE.md`; this file covers environment reality,
the release pipeline, and how agents should work in this repo. Keep it in sync
when facts change — don't let it drift into aspirational documentation.

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
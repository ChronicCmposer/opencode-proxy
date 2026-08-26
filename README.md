# opencode-proxy

Reach `opencode serve` running on your home Mac from anywhere — cellular
included — without port-forwarding your home router.

## How it works

The Mac can only make outbound connections, so the proxy inverts the usual
client/server direction: `opencode-proxy --local` dials **out** from the Mac
to a small AWS host and holds that connection open as a multiplexed tunnel
(WebSocket + [yamux](https://github.com/hashicorp/yamux)).
`opencode-proxy --remote` sits on the AWS host, takes requests from your
browser, and forwards each one down the tunnel to `opencode serve` on the Mac.

Every link is mutual TLS against a private CA you control — the browser must
present a device certificate, and the Mac must present a tunnel certificate,
before either can do anything. opencode's own password
(`OPENCODE_SERVER_PASSWORD`) is passed straight through end to end; the AWS
host never sees or stores it.

```
 phone (cellular) --mTLS--> AWS EC2 (--remote) <--mTLS-- MacBook (--local) --> opencode serve
```

## 1. Build

Both sides — `--local` and `--remote` — ship as the **same** `linux/arm64`
OCI container image, run under containerd via `ctr`. `--local` runs inside a
Debian VM (e.g. Parallels on the Mac); `--remote` runs on the EC2 host. No
native macOS binary is involved anywhere in this path.

```sh
make build   # native opencode-proxy binary, for local dev/testing only
make test    # unit + loopback integration tests
```

### Building the container image

Image builds go through **BuildKit's `buildctl`, talking to a standalone
`buildkitd` running the containerd worker** — no Docker CLI or daemon
anywhere in this path.

**Setting up buildkitd** (once per build machine): install `buildkitd` and
`buildctl` from the [BuildKit releases page](https://github.com/moby/buildkit/releases),
then run the daemon with the containerd worker enabled (this is what "depend
on buildkit and containerd rather than Docker" means in practice — BuildKit
executes build steps through containerd, and Docker never enters the
picture):

```sh
sudo containerd &                                  # if not already running
sudo buildkitd --containerd-worker=true &           # or install both as systemd units
```

Then build:

```sh
make image   # writes opencode-proxy.tar (an OCI image archive, linux/arm64)
```

`make image` runs `buildctl build --frontend dockerfile.v0 ... --output
type=oci,dest=opencode-proxy.tar` against `$BUILDKIT_HOST` (defaults to the
standard `unix:///run/buildkit/buildkitd.sock`; override if your daemon
listens elsewhere). The `Dockerfile` cross-compiles the Go binary for
`linux/arm64` inside the build (Go's native cross-compiler, no QEMU needed)
and copies just that static binary onto `FROM scratch` — the final image has
no shell, no package manager, nothing but `/opencode-proxy`.

### Versioning and releases

Every build embeds a version — `make build`/`make test`/`make image` all
depend on `make generate-version`, which writes `version.go`
from `git describe` (gitignored, regenerated every time; a plain `go build`
without going through `make` first will fail to compile, since that file
won't exist yet). On an exactly-tagged, clean commit this is just the tag
(e.g. `v1.2.3`); otherwise it's `v1.2.3-4-gabcdef-dirty` — still useful for
day-to-day dev builds.

Cutting an actual release is two commands. The very first tag has to be
created by hand (there's nothing to bump from yet):

```sh
git tag v0.1.0 && git push origin v0.1.0
```

After that:

```sh
make bump-version LEVEL=patch   # or minor / major — tags + pushes vX.Y.Z
make release                    # builds opencode-proxy.tar(.sha256) for
                                 # that exact tag and publishes a GitHub
                                 # Release with both attached (needs `gh`)
```

`make release` refuses to run unless the tree is clean, your local branch
matches `origin` exactly, HEAD is *exactly* the tag you just pushed, and
that tag actually exists on `origin` — it's meant to only ever build from a
commit someone else could check out and reproduce identically. The
CloudFormation stack's `ImageTarURL` parameter (and both hosts' upgrade
timers) point at `.../releases/latest/download/opencode-proxy.tar` and its
`.sha256`, which `make release` publishes with exactly those names.

## 2. Issue certificates

```sh
./pki/init-ca.sh                                  # once — creates pki/out/ca.{key,crt}
./pki/issue-server.sh code.example.com <eip>       # AWS host's TLS identity (run after step 3, once you have the EIP)
./pki/issue-tunnel.sh                              # the Mac's identity when dialing the tunnel
./pki/issue-client.sh phone                        # a browser device; repeat per device
```

`issue-client.sh` also emits `pki/out/phone.mobileconfig` — an Apple
configuration profile bundling CA trust and the device identity in one
install, and `phone.p12` for manual import elsewhere. The profile embeds the
p12 password in plaintext (Apple's format requires it), so treat the
`.mobileconfig` itself as a secret: transfer it, install it, then delete it.
See `pki/renew.sh` — server and tunnel certs are valid **90 days**, while
**device certs are valid 30 days** (they are the most-copied, highest-risk
credential, so their exposure window is kept short); the CA itself
(`init-ca.sh`) is valid **10 years** and isn't tracked by `renew.sh` — a
CA renewal means re-issuing every leaf cert too, so it's intentionally a
manual, rare operation rather than an automated one.

**Defense in depth for a lost device.** mTLS is the gate, but a stolen device
cert (or its `.mobileconfig`) otherwise grants full opencode — i.e. code
execution on the Mac — until it expires. Two independent controls limit that:
keep opencode's own `OPENCODE_SERVER_PASSWORD` set so a stolen cert alone
isn't enough (the password is passed end-to-end and never seen by the AWS
host), and revoke the cert's serial. Add the serial (from `openssl x509
-noout -serial`) to `/etc/opencode-proxy/revoked.txt` on the remote — it takes
effect at the **next handshake**, no restart or redeploy, so the lost device
is cut off immediately without rotating the whole CA.

**Keep `pki/out/ca.key` off any machine you don't fully trust.** It's the
root of everything reachable through the tunnel.

## 3. Deploy the AWS host

Requires the AWS CLI configured with credentials, and an EC2 key pair for
emergency SSH access. The instance launches on the **latest Amazon Linux
2023 arm64 AMI** (resolved fresh from an SSM public parameter on every
deploy, never pinned) and defaults to **`t4g.nano`** — plenty for one home's
tunnel plus occasional phone use; see the `InstanceType` parameter
description in `cloudformation/stack.yaml` for the sizing rationale and when
to bump it. UserData installs `containerd`, imports `opencode-proxy.tar`
(published in step 1) with `ctr images import`, and runs it under a systemd
unit wrapping `ctr run --net-host` — no Docker on the instance. It also
installs the `opencode-proxy-update.timer` that keeps the image current
going forward — see "Staying up to date" in step 4.

```sh
./pki/issue-server.sh code.example.com                       # before you have an EIP, SANs can be added later — see below
./cloudformation/deploy.sh opencode-proxy code.example.com my-ec2-key <your-home-ip>/32 v1.2.0
```

The last argument is the **repo ref** the instance fetches its unit files and
updater from at boot. You may pass a release tag (`vX.Y.Z`), a branch, or a
commit SHA — `deploy.sh` resolves it, on your trusted machine, to the full
commit SHA it currently names and pins **that** into the stack (the template
rejects anything but a 40-char SHA). Those files run as root at boot, and only
a commit SHA is content-addressed: unlike a tag, which can be force-moved on
GitHub to point at different content, a SHA fixes what runs to a commit you can
review, and it can't shift under a deployed stack. `deploy.sh` also passes the release signing **public key**
(`scripts/release-signing.pub`) into the stack as the `ReleaseSigningKey`
parameter — an out-of-band trust anchor. The image signature is only
meaningful if the key that verifies it can't be swapped alongside the image;
that's why the key comes from the template, not from the boot-time repo
tarball. `AdminCidr` (`<your-home-ip>/32`) is validated to reject `0.0.0.0/0`
and other internet-wide ranges — SSH is never open to the world.

`deploy.sh` prints the stack outputs, including the **Elastic IP**. Two
things then need that IP:

1. Re-issue the server cert with the IP as an extra SAN if you didn't
   already know it: `./pki/issue-server.sh code.example.com <eip>`
2. Point `code.example.com`'s A record at it in Namecheap (Domain List →
   Manage → Advanced DNS → Add A Record).

Then push the (possibly re-issued) cert material to SSM and let the
instance pick it up:

```sh
./cloudformation/upload-certs.sh
```

If you re-issued the cert after the instance already booted, SSH in
(`AdminCidr`-restricted) and re-run the fetch + `systemctl restart
opencode-proxy`, or just terminate the instance — the Elastic IP re-attaches
to whatever CloudFormation replaces it with next `deploy.sh` run.

## 4. Run `opencode serve` and the local proxy

`opencode serve` runs inside a Debian testing arm64 VM (Parallels, bridged
networking, containerd already installed). Deploy the **same**
`opencode-proxy.tar` image built in step 1 as a container there — no
separate local build, no Docker, just `ctr` like the EC2 side:

```sh
# on the VM, with the image tar and pki/out/{ca.crt,tunnel.crt,tunnel.key}
# already copied over (scp, or a Parallels shared folder):
sudo vm/deploy-local.sh opencode-proxy.tar pki/out wss://code.example.com/_tunnel
```

This installs containerd if it's missing, imports the image, and sets up an
`opencode-proxy-local` systemd unit running `ctr run --net-host` — the
container shares the VM's network namespace, so `--opencode-url
http://127.0.0.1:4096` (the default) reaches `opencode serve` running
natively in the same VM. `systemctl status opencode-proxy-local` /
`journalctl -u opencode-proxy-local -f` to check on it; `Restart=always`
means it survives VM reboots and reconnects after the Mac sleeps.

This script assumes the VM and `opencode serve` are already up — it doesn't
create the VM or install opencode.

### Staying up to date

Both `deploy.sh` (EC2) and `vm/deploy-local.sh` (VM) also install an
`opencode-proxy-update.timer` that runs every 6 hours on each host. Each
check downloads only the small `opencode-proxy.tar.sha256` file and
compares it to what's currently deployed (`/etc/opencode-proxy/current.sha256`)
— the full image is only pulled when that checksum actually changed. The
checksum is same-origin, so it proves only that the download wasn't corrupted,
**not** that it is authentic; before importing, the updater verifies the
maintainer's Ed25519 signature over the tar (`opencode-proxy.tar.sig`) against
the pinned public key at `/etc/opencode-proxy/release-signing.pub`, and
**refuses to import anything it can't verify** (see "Release signing" below).
On a verified change it snapshots the currently-running image under a rollback
tag, imports the new one, and restarts the service; if the service doesn't stay
up (`systemctl is-active`, checked 15s after restart), it automatically
re-tags and restarts back to the previous image. Either way you can see
what happened:

```sh
journalctl -u opencode-proxy-update      # what the last check(s) did
systemctl status opencode-proxy-update   # "failed" here means an update
                                          # was attempted and rolled back —
                                          # investigate before publishing
                                          # a fix, the host is still serving
                                          # traffic on the previous image
systemctl list-timers opencode-proxy-update.timer   # when it next runs
```

To publish an update: `make bump-version LEVEL=patch && make release` (see
"Versioning and releases" in step 1), then wait up to 6h — or force it
immediately with `sudo systemctl start opencode-proxy-update` on either
host.

### Release signing

The published image is code every host runs as root, so it is signed, not
just checksummed. One-time setup on a trusted machine:

```sh
scripts/init-signing-key.sh   # writes signing/release.key (keep offline,
                              # like pki/out/ca.key) and scripts/release-signing.pub
git add scripts/release-signing.pub && git commit -m "add release signing key"
```

`make release` then signs `opencode-proxy.tar` with the private key and
publishes `opencode-proxy.tar.sig` alongside it. Every host pins the public
key (`/etc/opencode-proxy/release-signing.pub`) and verifies the signature
before importing — at first boot and on every auto-update. On EC2 the key is
injected as the `ReleaseSigningKey` template parameter (from
`scripts/release-signing.pub`, passed by `deploy.sh`), **not** fetched from the
boot-time repo tarball: the key that verifies the image must be an immutable,
out-of-band anchor, or an attacker who can alter the repo could swap the key
and the image together and defeat the check. Verification is **fail-closed**:
until you replace the placeholder `scripts/release-signing.pub` with a real
key, deploys and updates refuse to import. Guard `signing/release.key` like the
CA key; losing it means re-running `init-signing-key.sh` and redeploying the
new public key, and leaking it lets an attacker forge releases.

> The systemd units and update script are fetched from
> `codeload.github.com/.../<RepoRef>` at deploy time, so `RepoRef`/`repo_ref`
> is required to be an immutable tag (`vX.Y.Z`) or full commit SHA — a branch
> like `main` is rejected — so that root-run path can't shift under a deployed
> stack.

### Checking versions from a response

Every response carries `X-Opencode-Proxy-Remote-Version`, stamped by the
EC2 host; a normal proxied response also carries
`X-Opencode-Proxy-Local-Version`, stamped by the VM and passed through
untouched (the 503-no-tunnel path only has the remote header, since no
response ever came from local):

```sh
curl -sI --cacert pki/out/ca.crt --cert pki/out/phone.crt --key pki/out/phone.key \
  https://code.example.com/ | grep -i x-opencode-proxy
```

Comparing the two after publishing an update confirms both hosts actually
picked it up — the 6h timers on EC2 and the VM run independently, so
there's a window where they can briefly disagree.

## 5. Connect from your phone

1. AirDrop (or otherwise transfer) `pki/out/phone.mobileconfig` to the
   phone and open it.
2. Settings → General → VPN & Device Management → install the profile.
3. Settings → General → About → Certificate Trust Settings → enable full
   trust for the "opencode-proxy CA" root.
4. **Delete the `.mobileconfig` file** afterward — it carries the device's
   private key and p12 password in plaintext.
5. On cellular, open `https://code.example.com`. Safari will prompt to pick
   a client certificate — choose the one you just installed — then present
   opencode's own login (the `OPENCODE_SERVER_PASSWORD` from step 4).

## Tuning

Reconnect backoff and the yamux session timeouts come from a JSON file passed
with `--config`. Both `deploy.sh` (EC2) and `vm/deploy-local.sh` (VM) install
`config.example.json` from the repo to `/etc/opencode-proxy/config.json`, and
both systemd units point `--config` at it:

```json
{
  "backoff-min": "1s",
  "backoff-max": "30s",
  "keepalive-interval": "30s",
  "stream-open-timeout": "2m"
}
```

| Key | What it controls |
| --- | --- |
| `backoff-min` | Delay before the local client's first reconnect attempt; each attempt after that doubles it. |
| `backoff-max` | Cap on the reconnect delay, jitter included. |
| `keepalive-interval` | How often an idle yamux session pings its peer, so a tunnel dropped by a NAT or load balancer is noticed rather than waited on. |
| `stream-open-timeout` | How long a stream may take to open. Generous by default: a browser's `GET /event` SSE stream sits idle-but-open for a long time. |

Every key is optional and layered over the built-in defaults shown above, so a
file setting one key leaves the rest alone — and omitting `--config` entirely is
valid. Values are Go duration strings (`"90s"`, `"2m"`, `"1h30m"`). Edit the
file and `systemctl restart opencode-proxy` (or `opencode-proxy-local`) to
apply.

The proxy refuses to start on a config it can't honour rather than falling back
silently — a non-positive duration, a `backoff-min` above `backoff-max`, an
unparseable duration, or a misspelled key each abort startup with the offending
key named. Check `journalctl -u opencode-proxy` if a unit won't come up after a
config edit.

## Verifying it end-to-end

- `make test` — unit tests plus a loopback integration test covering
  request/response round-trips, `Authorization` header passthrough,
  incremental SSE delivery (proves streaming isn't buffered), rejection of
  connections without a valid client cert, and the 503 returned when no
  tunnel is connected.
- Manually: with Wi-Fi off, confirm a chat response in the browser renders
  token-by-token rather than appearing all at once — that's the SSE path
  actually streaming end to end.
- Failure drills worth trying once: put the Mac to sleep and wake it (the
  tunnel should reconnect within ~30s); `systemctl stop opencode-proxy-local`
  on the VM (`Restart=always` should bring it back); reboot the EC2 instance
  (systemd should bring `--remote` back up and the VM should reconnect on
  its own).

## Renewing certificates

```sh
./pki/renew.sh          # lists anything expiring within 14 days
```

Re-run the matching `issue-*.sh` script, then redeploy that cert to wherever
it lives (SSM + instance restart for the server cert; re-run
`vm/deploy-local.sh`, or copy the renewed cert into `/etc/opencode-proxy` on
the VM and `systemctl restart opencode-proxy-local`, for the tunnel cert;
reinstall the `.mobileconfig` for a device cert).

## Revoking a compromised device

A lost or compromised device certificate doesn't require rotating the whole
CA. The remote reads a revocation list at `--revoked`
(`/etc/opencode-proxy/revoked.txt` on the EC2 host), one certificate serial
per line as hex; any listed serial is rejected at the TLS handshake, for
tunnel and device certs alike. Get a cert's serial with:

```sh
openssl x509 -in pki/out/phone.crt -noout -serial   # e.g. serial=1A2B3C...
```

Add that value (colons and a `serial=` prefix are fine — it's normalized) to
`revoked.txt` and restart the service; no redeploy or CA rotation needed. Full
CRL/OCSP responders are still not implemented — this is a static denylist you
maintain by hand.

## Out of scope

There's no rate limiting or WAF in front of the remote host beyond the mTLS
gate and the server's slow-header timeout (`read-header-timeout`), and this
only supports one home opencode instance per remote host.

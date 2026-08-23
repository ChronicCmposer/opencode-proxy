# opencode-proxy

Reach `opencode web` running on your home Mac from anywhere — cellular
included — without port-forwarding your home router.

## How it works

The Mac can only make outbound connections, so the proxy inverts the usual
client/server direction: `opencode-proxy --local` dials **out** from the Mac
to a small AWS host and holds that connection open as a multiplexed tunnel
(WebSocket + [yamux](https://github.com/hashicorp/yamux)).
`opencode-proxy --remote` sits on the AWS host, takes requests from your
browser, and forwards each one down the tunnel to `opencode web` on the Mac.

Every link is mutual TLS against a private CA you control — the browser must
present a device certificate, and the Mac must present a tunnel certificate,
before either can do anything. opencode's own password
(`OPENCODE_SERVER_PASSWORD`) is passed straight through end to end; the AWS
host never sees or stores it.

```
 phone (cellular) --mTLS--> AWS EC2 (--remote) <--mTLS-- MacBook (--local) --> opencode web
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
depend on `make generate-version`, which writes `internal/version/version.go`
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
install, and `phone.p12` for manual import elsewhere. See `pki/renew.sh` —
leaf certs are valid 90 days.

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
./cloudformation/deploy.sh opencode-proxy code.example.com my-ec2-key <your-home-ip>/32
```

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

## 4. Run `opencode web` and the local proxy

`opencode web` runs inside a Debian testing arm64 VM (Parallels, bridged
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
http://127.0.0.1:4096` (the default) reaches `opencode web` running
natively in the same VM. `systemctl status opencode-proxy-local` /
`journalctl -u opencode-proxy-local -f` to check on it; `Restart=always`
means it survives VM reboots and reconnects after the Mac sleeps.

This script assumes the VM and `opencode web` are already up — it doesn't
create the VM or install opencode.

### Staying up to date

Both `deploy.sh` (EC2) and `vm/deploy-local.sh` (VM) also install an
`opencode-proxy-update.timer` that runs every 6 hours on each host. Each
check downloads only the small `opencode-proxy.tar.sha256` file and
compares it to what's currently deployed (`/etc/opencode-proxy/current.sha256`)
— the full image is only pulled when that checksum actually changed. On a
change it snapshots the currently-running image under a rollback tag,
imports the new one, and restarts the service; if the service doesn't stay
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

## Out of scope

Certificate revocation (CRL/OCSP) isn't implemented — a compromised device
cert must be handled by re-issuing the CA and every other cert. There's no
rate limiting or WAF in front of the remote host beyond the mTLS gate, and
this only supports one home opencode instance per remote host.

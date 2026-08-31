#!/usr/bin/env bash
# Usage: issue-client.sh <device-name>   (e.g. "phone", "ipad")
#
# Also emits <name>.mobileconfig (an Apple profile bundling CA trust + device
# identity in one install, so Safari stops prompting for the cert). The
# intermediate <name>.p12 is an input to that profile, not a deliverable —
# it's shredded on exit.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <device-name>" >&2
  exit 1
fi
name="$1"

# 30-day lifetime (vs. the 90-day default for server/tunnel): a device cert is
# the most exposed credential — it lives on a phone and inside an AirDropped,
# plaintext-password .mobileconfig — so keep the window a leaked one stays
# valid short. Rotate with `issue-client.sh <name>`; `renew.sh` flags it early.
issue_leaf "$name" "$name" "$OU_DEVICE" "clientAuth" "" 30

key="$OUT_DIR/$name.key"
crt="$OUT_DIR/$name.crt"
p12="$OUT_DIR/$name.p12"
mobileconfig="$OUT_DIR/$name.mobileconfig"

# The p12 password is the highest-value secret this script mints. It is never
# written to its own sidecar file (the old <name>.p12.password lingered in
# pki/out/ as a plaintext credential): it lives only in this shell and, because
# Apple's profile format requires it, inside the .mobileconfig. The intermediate
# .p12 is an input to the profile, not a deliverable, so it's shredded on exit —
# a normal EXIT after the profile is built, or an early one on error — leaving
# only the .mobileconfig (itself a secret) behind. shred is best-effort:
# fall back to rm where it isn't available (e.g. macOS).
shred_or_rm() {
  for f in "$@"; do
    [[ -e "$f" ]] || continue
    shred -u "$f" 2>/dev/null || rm -f "$f"
  done
}
trap 'shred_or_rm "$p12"' EXIT

p12pass="$(openssl rand -base64 18)"
# Apple's profile installer parses this embedded p12 with the same
# SecKeychainItemImport machinery that rejects some OpenSSL 3.x default cipher
# suites — surfacing as "MAC verification failed" / "The certificate could not
# be verified". PBE-SHA1-3DES plus an SHA-1 MAC is the maximally-compatible
# combination for both LibreSSL (macOS's system openssl) and OpenSSL 3.x.
openssl pkcs12 -export -inkey "$key" -in "$crt" -certfile "$CA_CERT" \
  -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1 \
  -name "$name" -passout "pass:$p12pass" -out "$p12"
chmod 600 "$p12"

gen_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen
  elif [[ -r /proc/sys/kernel/random/uuid ]]; then
    cat /proc/sys/kernel/random/uuid
  else
    python3 -c "import uuid; print(uuid.uuid4())"
  fi
}

ca_der_b64="$(openssl x509 -in "$CA_CERT" -outform DER | base64 | tr -d '\n')"
p12_b64="$(base64 < "$p12" | tr -d '\n')"
root_uuid="$(gen_uuid)"
identity_uuid="$(gen_uuid)"
profile_uuid="$(gen_uuid)"

cat > "$mobileconfig" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key><string>com.apple.security.root</string>
      <key>PayloadIdentifier</key><string>ai.opencode.proxy.ca</string>
      <key>PayloadUUID</key><string>${root_uuid}</string>
      <key>PayloadVersion</key><integer>1</integer>
      <key>PayloadDisplayName</key><string>opencode-proxy CA</string>
      <key>PayloadCertificateFileName</key><string>ca.cer</string>
      <key>PayloadContent</key><data>${ca_der_b64}</data>
    </dict>
    <dict>
      <key>PayloadType</key><string>com.apple.security.pkcs12</string>
      <key>PayloadIdentifier</key><string>ai.opencode.proxy.identity.${name}</string>
      <key>PayloadUUID</key><string>${identity_uuid}</string>
      <key>PayloadVersion</key><integer>1</integer>
      <key>PayloadDisplayName</key><string>opencode-proxy device: ${name}</string>
      <key>PayloadCertificateFileName</key><string>${name}.p12</string>
      <key>PayloadContent</key><data>${p12_b64}</data>
      <key>Password</key><string>${p12pass}</string>
    </dict>
  </array>
  <key>PayloadDisplayName</key><string>opencode-proxy: ${name}</string>
  <key>PayloadIdentifier</key><string>ai.opencode.proxy.profile.${name}</string>
  <key>PayloadUUID</key><string>${profile_uuid}</string>
  <key>PayloadVersion</key><integer>1</integer>
  <key>PayloadType</key><string>Configuration</string>
  <key>PayloadRemovalDisallowed</key><false/>
</dict>
</plist>
PLIST
chmod 600 "$mobileconfig"

echo "issued: $mobileconfig"
echo "AirDrop or otherwise transfer this file to the device and open it to install."
echo "The profile embeds the p12 password in plaintext (Apple's format requires this)"
echo "— treat the .mobileconfig itself as a secret and delete it after installing."
echo "After install: Settings > General > VPN & Device Management > install the profile,"
echo "then Settings > General > About > Certificate Trust Settings > enable full trust"
echo "for the opencode-proxy CA."
echo "macOS: install via System Settings > General > VPN & Device Management (accept 'not signed'),"
echo "then Keychain Access > set the opencode-proxy CA to Always Trust."

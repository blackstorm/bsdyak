#!/usr/bin/env bash
set -euo pipefail

FREEBSD_VERSION="${FREEBSD_VERSION:-14.5-RELEASE}"
IMAGE_NAME="FreeBSD-${FREEBSD_VERSION}-amd64-BASIC-CLOUDINIT-zfs.qcow2"
IMAGE_URL="https://download.freebsd.org/releases/VM-IMAGES/${FREEBSD_VERSION}/amd64/Latest/${IMAGE_NAME}.xz"
CACHE_DIR="${YAK_E2E_CACHE:-${HOME}/.cache/yak-e2e}"
BASE_QCOW2="${CACHE_DIR}/${IMAGE_NAME}"
WARMED_QCOW2="${CACHE_DIR}/FreeBSD-${FREEBSD_VERSION}-amd64-yak-warmed.qcow2"
E2E_KEY="${CACHE_DIR}/e2e_id_ed25519"
SSH_PORT="${YAK_E2E_SSH_PORT:-2222}"

for bin in qemu-system-x86_64 qemu-img xorriso xz curl ssh scp go; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "missing required tool: $bin" >&2
    exit 1
  fi
done

WORKDIR="$(mktemp -d -t yak-freebsd-e2e-XXXXXX)"
QEMU_PID=""

cleanup() {
  if [[ -n "${QEMU_PID}" ]] && kill -0 "${QEMU_PID}" >/dev/null 2>&1; then
    kill "${QEMU_PID}" >/dev/null 2>&1 || true
    wait "${QEMU_PID}" >/dev/null 2>&1 || true
  fi
  rm -f "${WARMED_QCOW2}.tmp"
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

mkdir -p "${CACHE_DIR}"
if [[ ! -f "${BASE_QCOW2}" ]]; then
  echo "==> Downloading ${IMAGE_URL}..."
  curl -fSL "${IMAGE_URL}" -o "${BASE_QCOW2}.xz.tmp"
  xz -dc "${BASE_QCOW2}.xz.tmp" > "${BASE_QCOW2}.tmp"
  rm -f "${BASE_QCOW2}.xz.tmp"
  mv "${BASE_QCOW2}.tmp" "${BASE_QCOW2}"
fi

if [[ ! -f "${E2E_KEY}" ]]; then
  ssh-keygen -q -t ed25519 -N "" -f "${E2E_KEY}"
fi
chmod 600 "${E2E_KEY}"
PUBKEY="$(cat "${E2E_KEY}.pub")"

SSH_OPTS=(
  -i "${E2E_KEY}"
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o ConnectTimeout=3
  -p "${SSH_PORT}"
)

ACCEL_ARGS=()
if [[ -r /dev/kvm && -w /dev/kvm ]]; then
  ACCEL_ARGS=(-enable-kvm -cpu host)
fi

wait_for_ssh() {
  local label="$1"
  local log_file="$2"
  local max_iters="${3:-60}" # 60 * 2s = 120s
  local ready=0

  echo "==> Waiting for ${label} SSH readiness (up to $((max_iters * 2))s)..."
  for i in $(seq 1 "${max_iters}"); do
    if ssh "${SSH_OPTS[@]}" root@127.0.0.1 "true" >/dev/null 2>&1; then
      ready=1
      break
    fi
    if ! kill -0 "${QEMU_PID}" >/dev/null 2>&1; then
      echo "QEMU (${label}) exited unexpectedly. Console log:" >&2
      tail -n 60 "${log_file}" >&2 || true
      return 1
    fi
    if (( i % 5 == 0 )); then
      local last_line
      last_line="$(tr -d '\r' < "${log_file}" 2>/dev/null | awk 'NF{line=$0} END{print line}')"
      echo "    ...waiting ($((i * 2))s): ${last_line}"
    fi
    sleep 2
  done

  if [[ "${ready}" -ne 1 ]]; then
    echo "Timed out waiting for ${label} SSH. Console log:" >&2
    tail -n 80 "${log_file}" >&2 || true
    return 1
  fi
}

# One-time offline warm pass disables firstboot_freebsd_update & bakes SSH + pf.conf
# into a cached base image so every test & CI run boots in ~10-15s.
if [[ ! -f "${WARMED_QCOW2}" ]]; then
  echo "==> Warming cached FreeBSD ${FREEBSD_VERSION} base image (one-time offline boot)..."
  rm -f "${WARMED_QCOW2}.tmp"
  qemu-img create -q -f qcow2 -F qcow2 -b "${BASE_QCOW2}" "${WARMED_QCOW2}.tmp" 20G

  mkdir -p "${WORKDIR}/cidata"
  cat > "${WORKDIR}/cidata/meta-data" <<EOF
instance-id: yak-e2e-warm
local-hostname: yak-freebsd-e2e
EOF

  cat > "${WORKDIR}/cidata/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - ${PUBKEY}
write_files:
  - path: /etc/pf.conf
    content: |
      set skip on lo0
      nat-anchor "yak/*"
      anchor "yak/*"
      pass in all
      pass out all
    permissions: '0644'
    owner: root:wheel
  - path: /etc/ssh/yak_e2e.pub
    content: |
      ${PUBKEY}
    permissions: '0644'
    owner: root:wheel
runcmd:
  - "mkdir -p /root/.ssh && chmod 700 /root/.ssh"
  - "cp /etc/ssh/yak_e2e.pub /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys"
  - "sed -i '' -e 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config"
  - "grep -q '^PermitRootLogin prohibit-password' /etc/ssh/sshd_config || echo 'PermitRootLogin prohibit-password' >> /etc/ssh/sshd_config"
  - "service sshd reload || service sshd restart"
  - "sysrc gateway_enable=YES pf_enable=YES sshd_enable=YES"
EOF

  xorriso -as mkisofs -V cidata -J -R -o "${WORKDIR}/cidata.iso" "${WORKDIR}/cidata" >/dev/null 2>&1

  qemu-system-x86_64 \
    "${ACCEL_ARGS[@]}" \
    -m 2048 \
    -smp 2 \
    -nographic \
    -serial "file:${WORKDIR}/warm-console.log" \
    -drive "file=${WARMED_QCOW2}.tmp,format=qcow2,if=virtio" \
    -cdrom "${WORKDIR}/cidata.iso" \
    -netdev "user,id=net0,restrict=on,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22" \
    -device virtio-net-pci,netdev=net0 &
  QEMU_PID=$!

  wait_for_ssh "warm-image" "${WORKDIR}/warm-console.log" 90

  echo "==> Sealing warmed FreeBSD base image..."
  ssh "${SSH_OPTS[@]}" root@127.0.0.1 <<'SEAL'
set -eu
sysrc firstboot_freebsd_update_enable="NO" firstboot_pkgs_enable="NO" firstboot_pkg_upgrade_enable="NO" gateway_enable="YES" pf_enable="YES" sshd_enable="YES"
rm -f /firstboot
sync
shutdown -p now || poweroff
SEAL
  wait "${QEMU_PID}" >/dev/null 2>&1 || true
  QEMU_PID=""
  mv "${WARMED_QCOW2}.tmp" "${WARMED_QCOW2}"
fi

echo "==> Preparing ephemeral overlay disk from warmed image..."
qemu-img create -q -f qcow2 -F qcow2 -b "${WARMED_QCOW2}" "${WORKDIR}/overlay.qcow2" 20G

echo "==> Booting ephemeral FreeBSD ${FREEBSD_VERSION} VM on 127.0.0.1:${SSH_PORT}..."
qemu-system-x86_64 \
  "${ACCEL_ARGS[@]}" \
  -m 2048 \
  -smp 2 \
  -nographic \
  -serial "file:${WORKDIR}/console.log" \
  -drive "file=${WORKDIR}/overlay.qcow2,format=qcow2,if=virtio" \
  -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22" \
  -device virtio-net-pci,netdev=net0 &
QEMU_PID=$!

wait_for_ssh "e2e-vm" "${WORKDIR}/console.log" 60

echo "==> Cross-compiling yak for freebsd/amd64 and provisioning VM..."
mkdir -p dist
CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -o dist/yak-freebsd-amd64 ./cmd/yak
CGO_ENABLED=0 go build -o dist/yak ./cmd/yak

ssh "${SSH_OPTS[@]}" root@127.0.0.1 "mkdir -p /usr/local/bin /usr/local/etc/rc.d"
scp -i "${E2E_KEY}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -P "${SSH_PORT}" \
  dist/yak-freebsd-amd64 root@127.0.0.1:/usr/local/bin/yak

ssh "${SSH_OPTS[@]}" root@127.0.0.1 <<'REMOTE'
set -eu
chmod 0755 /usr/local/bin/yak
ZPOOL="$(zpool list -H -o name | head -n 1)"
EGRESS="$(route -n get default 2>/dev/null | awk '/interface:/{print $2}' || true)"
if [ -z "${EGRESS}" ]; then
  EGRESS="vtnet0"
fi
yak server init --zfs-root "${ZPOOL}/yak" --egress "${EGRESS}"
yak server check
sysrc yakd_enable="YES"
service yakd start
REMOTE

echo "==> Running FreeBSD kernel E2E test suite..."
export YAK_E2E_HOST="root@127.0.0.1:${SSH_PORT}"
export YAK_E2E_SSH_KEY="${E2E_KEY}"
export YAK_E2E_FREEBSD_VERSION="${FREEBSD_VERSION}"
export YAK_BIN="$(pwd)/dist/yak"
go test -tags freebsd_e2e -v ./tests/e2e

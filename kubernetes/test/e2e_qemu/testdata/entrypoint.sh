#!/usr/bin/env bash
# Copyright 2026 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

runtime_dir=/run/qemu-e2e
machine_type=pc-q35-6.2
vcpus=2
memory_mb=512
memory_bytes=$((memory_mb * 1024 * 1024))

mkdir -p "$runtime_dir"
rm -f "$runtime_dir/qmp.sock" "$runtime_dir/guest.sock"

cat >"$runtime_dir/launch.json" <<EOF
{
  "formatVersion": "qemu-v1",
  "architecture": "amd64",
  "qemuVersion": "6.2.0",
  "machineType": "$machine_type",
  "cpuModel": "host",
  "vcpus": $vcpus,
  "memoryBytes": $memory_bytes,
  "qemuConfigDigest": "sha256:ff11a22e7b6607bebb129784602b99c44e05fb3fb1b494499aa0b962582970aa",
  "disks": [
    {
      "id": "osdisk",
      "overlayPath": "/vm/state.qcow2",
      "capture": "rootfs"
    }
  ]
}
EOF

qemu_args=(
  -name opensandbox-qemu-vmstate-e2e
  -machine "$machine_type,accel=kvm"
  -cpu host
  -smp "$vcpus"
  -m "$memory_mb"
  -kernel /vm/vmlinuz
  -initrd /vm/initramfs.cpio.gz
  -append "console=ttyS0 rdinit=/init panic=-1 quiet"
  -blockdev driver=file,filename=/vm/state.qcow2,node-name=osfile
  -blockdev driver=qcow2,file=osfile,node-name=osdisk
  -device virtio-blk-pci,drive=osdisk
  -netdev user,id=net0,hostfwd=tcp:127.0.0.1:18080-:8080
  -device virtio-net-pci,netdev=net0
  -display none
  -no-reboot
  -qmp "unix:$runtime_dir/qmp.sock,server=on,wait=off"
  -serial "unix:$runtime_dir/guest.sock,server=on,wait=off"
)

if [[ "${OPENSANDBOX_RESTORE_MODE:-}" == "qemu-v1" ]]; then
  if [[ -z "${OPENSANDBOX_VMSTATE_DIR:-}" || ! -x "$OPENSANDBOX_VMSTATE_DIR/vmstate-loader" ]]; then
    echo "OpenSandbox VMState restore directory is incomplete" >&2
    exit 1
  fi
  qemu_args+=(
    -incoming
    "exec:$OPENSANDBOX_VMSTATE_DIR/vmstate-loader stream --dir $OPENSANDBOX_VMSTATE_DIR"
  )
fi

qemu-system-x86_64 "${qemu_args[@]}" &
qemu_pid=$!

terminate() {
  kill -TERM "$qemu_pid" 2>/dev/null || true
  wait "$qemu_pid" 2>/dev/null || true
}
trap terminate TERM INT EXIT

for _ in $(seq 1 300); do
  [[ -S "$runtime_dir/qmp.sock" ]] && break
  if ! kill -0 "$qemu_pid" 2>/dev/null; then
    wait "$qemu_pid"
    exit 1
  fi
  sleep 0.1
done

if [[ ! -S "$runtime_dir/qmp.sock" ]]; then
  echo "QMP socket was not created" >&2
  exit 1
fi

wait "$qemu_pid"

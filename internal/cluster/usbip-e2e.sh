#!/bin/sh
set -eux

test "$(id -u)" != 0
test -x /sbin/udevd
pgrep -x udevd >/dev/null
test "$(uname -r)" = "${CODESPACE_E2E_USBIP_KERNEL_RELEASE:?}"
test -c /dev/ptmx
test -d /dev/shm
mountpoint -q /dev/mqueue

if test "${1:-}" = check-disconnected; then
  for i in $(seq 1 30); do
    present=false
    for path in /dev/codespace-usbip/ttyACM*; do test -c "$path" && present=true; done
    if test "$present" = false; then exit 0; fi
    sleep 1
  done
  echo "CDC ACM device remained after SSH disconnect" >&2
  exit 1
fi

if test "${1:-}" = check-lab; then
  gitea-codespace-usbip list --format json | python3 -c '
import json, sys
value = json.load(sys.stdin)
assert [source["name"] for source in value["sources"]] == ["lab"]
assert any(device["source"] == "lab" and device["busId"] == "1-1" for device in value["devices"])
'
  exit 0
fi

gitea-codespace-usbip list --format json | python3 -c '
import json, sys
value = json.load(sys.stdin)
assert value["version"] == 1
assert {source["name"] for source in value["sources"]} == {"workstation", "lab"}
assert {(device["source"], device["busId"]) for device in value["devices"]} == {("workstation", "1-1"), ("lab", "1-1")}
'
# The ordinary reverse listener must be reachable inside this Dev Container.
python3 -c 'import socket; socket.create_connection(("127.0.0.1", 3240), timeout=5).close()'
gitea-codespace-usbip attach 1-1 --source lab
# The kernel enumerates an attached device asynchronously.
python3 - <<'PY'
import json
import subprocess
import time

deadline = time.monotonic() + 30
while time.monotonic() < deadline:
    status = json.loads(subprocess.check_output(["gitea-codespace-usbip", "status", "--format", "json"]))
    device = next((device for device in status["devices"] if device["source"] == "lab" and device["busId"] == "1-1"), None)
    if device is not None:
        subprocess.run(["gitea-codespace-usbip", "detach", str(device["port"])], check=True)
        break
    time.sleep(0.2)
else:
    raise SystemExit("USB/IP device from lab was not enumerated")
PY
for i in $(seq 1 30); do
  present=false
  for path in /dev/codespace-usbip/ttyACM*; do test -c "$path" && present=true; done
  if test "$present" = false; then break; fi
  sleep 1
done
test "$present" = false
gitea-codespace-usbip attach 1-1 --source workstation
gitea-codespace-usbip list

device=
for i in $(seq 1 30); do
  for path in /dev/codespace-usbip/ttyACM*; do
    if test -c "$path"; then
      device=$path
      break 2
    fi
  done
  sleep 1
done
test -n "$device"

python3 - <<'PY'
import ctypes
import errno
import os

master, slave = os.openpty()
try:
    expected = b"codespace-pty\n"
    os.write(master, expected)
    if os.read(slave, len(expected)) != expected:
        raise SystemExit("PTY read/write failed")
finally:
    os.close(master)
    os.close(slave)

shm_path = "/dev/shm/codespace-usbip-e2e"
fd = os.open(shm_path, os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
try:
    expected = b"codespace-shm"
    os.write(fd, expected)
    os.lseek(fd, 0, os.SEEK_SET)
    if os.read(fd, len(expected)) != expected:
        raise SystemExit("shm read/write failed")
finally:
    os.close(fd)
    os.unlink(shm_path)

libc = ctypes.CDLL(None, use_errno=True)
libc.mq_open.argtypes = [ctypes.c_char_p, ctypes.c_int, ctypes.c_uint, ctypes.c_void_p]
libc.mq_open.restype = ctypes.c_int
libc.mq_close.argtypes = [ctypes.c_int]
libc.mq_unlink.argtypes = [ctypes.c_char_p]
name = b"/codespace-usbip-e2e"
if libc.mq_unlink(name) != 0 and ctypes.get_errno() != errno.ENOENT:
    raise OSError(ctypes.get_errno(), "mq_unlink")
queue = libc.mq_open(name, os.O_CREAT | os.O_RDWR, 0o600, None)
if queue < 0:
    raise OSError(ctypes.get_errno(), "mq_open")
if libc.mq_close(queue) != 0:
    raise OSError(ctypes.get_errno(), "mq_close")
if libc.mq_unlink(name) != 0:
    raise OSError(ctypes.get_errno(), "mq_unlink")
PY

python3 - "$device" <<'PY'
import os
import select
import sys
import termios
import time

fd = os.open(sys.argv[1], os.O_RDWR | os.O_NOCTTY)
try:
    attrs = termios.tcgetattr(fd)
    attrs[0] = 0
    attrs[1] = 0
    attrs[2] |= termios.CLOCAL | termios.CREAD
    attrs[3] = 0
    termios.tcsetattr(fd, termios.TCSANOW, attrs)
    expected = b"codespace-usbip-e2e\n"
    os.write(fd, expected)
    received = bytearray()
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline and expected not in received:
        readable, _, _ = select.select([fd], [], [], 0.5)
        if readable:
            received.extend(os.read(fd, 4096))
    if expected not in received:
        raise SystemExit("CDC ACM echo did not return the written bytes: %r" % bytes(received))
finally:
    os.close(fd)
PY

status=$(gitea-codespace-usbip status --format json)
printf '%s\n' "$status"
usbip_port=$(printf '%s\n' "$status" | python3 -c '
import json, sys
value = json.load(sys.stdin)
assert value["version"] == 1
device = next(device for device in value["devices"] if device["source"] == "workstation" and device["busId"] == "1-1")
assert device["state"] == "Port in Use"
print(device["port"])
')
test -n "$usbip_port"
gitea-codespace-usbip detach "$usbip_port"
for i in $(seq 1 30); do
  present=false
  for path in /dev/codespace-usbip/ttyACM*; do test -c "$path" && present=true; done
  if test "$present" = false; then exit 0; fi
  sleep 1
done
echo "CDC ACM device remained after USB/IP detach" >&2
exit 1

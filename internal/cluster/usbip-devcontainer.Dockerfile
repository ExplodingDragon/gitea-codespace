FROM docker.io/library/alpine:3.22

RUN apk add --no-cache eudev python3 util-linux \
    && (addgroup -S dialout || true) \
    && addgroup -g 1000 vscode \
    && adduser -D -u 1000 -G vscode -s /bin/sh vscode \
    && addgroup vscode dialout \
    && mkdir -p /run/udev /etc/udev/rules.d \
    && printf '%s\n' 'SUBSYSTEM=="tty", KERNEL=="ttyACM[0-9]*", GROUP="dialout", MODE="0660", SYMLINK+="codespace-usbip/%k"' > /etc/udev/rules.d/99-codespace-usbip.rules \
    && printf '%s\n' '#!/bin/sh' 'set -eu' 'mkdir -p /run/udev' 'udevd --daemon' 'udevadm trigger || true' 'udevadm settle || true' 'exec "$@"' > /usr/local/bin/codespace-entrypoint \
    && chmod 0555 /usr/local/bin/codespace-entrypoint

USER root
ENTRYPOINT ["/usr/local/bin/codespace-entrypoint"]
CMD ["sleep", "infinity"]

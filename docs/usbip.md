# Connect USB devices

Compatible Kata environments let a Codespace use USB devices exported by a
workstation or device server. The connection uses the Codespace's authenticated
SSH endpoint. Attached devices appear at their normal `/dev` paths and are
shared by processes in that workspace.

## Before you connect

- Select a Kata environment prepared for USB/IP. The [operator guide](../charts/gitea-codespace/README.md#enable-kata-usbip)
  covers guest kernel and runtime requirements.
- Start a USB/IP server on the machine that owns the device and export the
  intended device using that server's tools.
- Use a development image with the drivers' userspace tools and, when needed,
  udev or eudev rules for non-root device access.
- Obtain the SSH user, host, and port from the Codespace page in Gitea.

The bundled IDEs show **USB Devices** when the runtime provides USB/IP support.
The Dev Container also receives `gitea-codespace-usbip` for command-line use.

## Connect a source

Run this command on the workstation and keep it running:

```sh
ssh -NT -o ExitOnForwardFailure=yes \
  -R usbip.workstation:3240:127.0.0.1:3240 <user>@<host>
```

Add `-p <port>` when the Codespace SSH endpoint uses a port other than 22.
The final `127.0.0.1:3240` is the USB/IP server as reached from your SSH client;
replace it with the device server's address and port when necessary.

`workstation` is a source name unique within this Codespace. Use 1-32 lowercase
letters, digits or hyphens, starting and ending with a letter or digit. The
`usbip.<source>:3240` part identifies a logical USB/IP binding, leaving Dev
Container ports available. Ordinary SSH `-R` bindings listen inside the Dev
Container instead.

For a second server, add another binding to the command:

```sh
-R usbip.lab:3240:<device-server>:<port>
```

Source registration confirms that the SSH listener exists. Listing devices
also checks whether the remote USB/IP server is responding.

## Attach and detach devices

Inside the Dev Container, list sources and their exported devices:

```sh
gitea-codespace-usbip sources
gitea-codespace-usbip list
gitea-codespace-usbip list --source workstation
```

Attach a bus ID from the list, then inspect the imported device:

```sh
gitea-codespace-usbip attach <bus-id> --source workstation
gitea-codespace-usbip status
```

The source can be omitted when exactly one is connected. With multiple sources,
select both the source and bus ID because different servers can export the
same bus ID. To detach, use the local VHCI port reported by `status`, which can
be zero:

```sh
gitea-codespace-usbip detach <port>
```

Detaching interrupts applications using the device. After a timeout, inspect
`status` before repeating an attach or detach because the action may already
have completed.

The bundled [VS Code](../providers/vscode/README.md#usb-devices) and
[DeepSeek Harness](../providers/dsh/README.md#usb-devices) plugins provide the
same operations. DSH also exposes conversation commands and tools.

## Use an attached device

The guest kernel supplies drivers; your development image supplies application
tools and device permissions. Common paths are:

| Use | Device path and preparation |
| --- | --- |
| ADB, DFU, libusb debug probes | `/dev/bus/usb`; install the application in your image and grant the remote user access through device rules |
| Serial console | `/dev/ttyACM*` or `/dev/ttyUSB*`; configure the user's serial-device group or udev rules |
| HID debug probe | `/dev/hidraw*`; use the matching application and device rules |
| USB disk | `/dev/sd*` and partition nodes; the container configuration must grant block-device access and mount capability |
| I2C, SPI, GPIO bridge | `/dev/i2c-*`, `/dev/spidev*`, `/dev/gpiochip*`; paths depend on the bridge driver and child-device binding |
| USBTMC instrument | `/dev/usbtmc*`; use an application that supports the instrument protocol |

Mount filesystems inside the Dev Container so they are visible in its mount
namespace. Check the operator guide for the supplied filesystem drivers;
filesystem repair tools belong in your development image. Identify the disk
and unmount it before detaching to protect its data.

CH341 SPI support registers an SPI controller. A `/dev/spidev*` node also needs
the intended child device to bind to spidev, using the standard Linux SPI
binding procedure for that hardware.

An ADB device also needs debugging enabled and authorization on the device.
The Runtime's USB database supplies device names when the vendor/product ID is
known; device access depends on drivers and permissions, rather than the
display name.

## Disconnect and reconnect

Closing a source's SSH forward disconnects its devices while other sources
remain active. Closing the SSH session releases all forwards owned by that
session. After reconnecting SSH or restarting the Codespace, list the devices
and attach them again by their current bus IDs.

If the USB command or IDE view is absent, ask the operator to check the Kata
environment's USB/IP capability. If listing succeeds but an application cannot
use the device, check its guest driver, development-image tools, and device
ownership or udev rules. Kernel support and userspace permissions serve
different purposes, so a successful attach alone does not verify application
access.

Protect the device server's USB/IP endpoint as well as the SSH connection.
Workspace users and applications share the attached hardware; use the
[operator guide](../charts/gitea-codespace/README.md#enable-kata-usbip) when
reviewing the deployment's device isolation boundary.

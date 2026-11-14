# Connect USB devices

Compatible Kata environments let a Codespace use USB devices exported by a
workstation or device server. The connection uses the Codespace's authenticated
SSH endpoint. Attached devices appear at their normal `/dev` paths in the main
Dev Container and can be used by processes with the required device permissions.

## Before you connect

- Select a Kata environment prepared for USB/IP. The [operator guide](../charts/gitea-codespace/README.md#enable-kata-usbip)
  covers guest kernel and runtime requirements.
- Start a USB/IP server on the machine that owns the device and export the
  intended device using that server's tools.
- Use a development image with the drivers' userspace tools and, when needed,
  udev or eudev rules for non-root device access.
- Obtain the SSH user, host, and port from the Codespace page in Gitea, or use
  the bundled IDE's USB tunnel command described below.

The bundled IDEs show **USB Devices** when the runtime provides USB/IP support.
The Dev Container also receives `gitea-codespace-usbip` for command-line use.

## Connect a source

In VS Code's USB view, choose **Copy USB tunnel command**. In DSH, open
**Codespace → Connection help** and use the USB/IP section. Choose a source name
(default: `workstation`). The command includes the current workspace's actual
SSH connection details. Run it on the workstation and keep it running. In DSH,
the generated command remains available for manual copying if the browser
cannot access the clipboard.

To enter the connection details yourself:

```sh
ssh -NT -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
  -R usbip.workstation:3240:127.0.0.1:3240 -p <port> -l <user> <host>
```

The final `127.0.0.1:3240` is the USB/IP server as reached from your SSH client;
replace it with the device server's address and port when necessary.
The [SSH guide](ssh.md) describes the connection variables, their refresh timing,
and other SSH uses. The tunnel command belongs on the workstation because its
SSH client must reach that USB/IP server.

The keepalive options maintain SSH activity while USB devices are idle and let
the client detect an unresponsive Gateway after about 90 seconds. They do not
reconnect a closed tunnel; run the command again to reconnect. The Gateway's
configured SSH idle timeout must exceed the 30-second keepalive interval.

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

`list` queries remote exports, while `status` reads local imports. A listed
device can become occupied or disconnected before you attach it. JSON output
(`--format json`) keeps each source's error in `sources[].error` alongside
successful results, so an empty device array alone does not mean every source
was queried successfully.

Attach a bus ID from the list, then inspect the imported device:

```sh
gitea-codespace-usbip attach <bus-id> --source workstation
gitea-codespace-usbip status
```

The source can be omitted when exactly one is connected. With multiple sources,
select both the source and bus ID because different servers can export the
same bus ID. Successful import hands the device to the guest kernel; device
enumeration and application setup may still be pending. To detach, use the
local VHCI port reported by `status`, which can be zero and is not a TCP port:

```sh
gitea-codespace-usbip detach <port>
```

Detaching interrupts applications using the device; unmount disks first. After
a timeout, inspect `status` before repeating an attach or detach because the
action may already have completed.

The bundled [VS Code](../providers/vscode/README.md#usb-devices) and
[DeepSeek Harness](../providers/dsh/README.md#usb-devices) plugins provide the
same operations. Both confirm a UI detach and refresh status after an operation.
If status cannot be refreshed, the previous data remains visible but device
actions wait for a successful refresh. DSH also exposes conversation commands
and tools through the same non-interactive CLI.

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

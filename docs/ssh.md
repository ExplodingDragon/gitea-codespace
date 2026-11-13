# Connect over SSH

The Codespace page in Gitea shows the SSH connection details. The bundled
VS Code extension and DeepSeek Harness plugin also provide **Copy SSH command**
in their Ports interface. Run the copied command on your local computer with
an SSH key registered in Gitea. The connection uses the existing Gateway
authentication and SSH host-key verification.

The SSH login selects the Codespace; commands run as its Dev Container remote
user. It is different from both your Gitea username and the container account.

## Connection information inside the Dev Container

The platform supplies these variables to SSH sessions, Web IDE processes,
lifecycle commands, login shells, and systemd system services:

| Variable | Value |
| --- | --- |
| `CODESPACE_SSH_HOST` | Public Gateway DNS name, without a scheme or port |
| `CODESPACE_SSH_PORT` | Public SSH port as a decimal number |
| `CODESPACE_SSH_USER` | `cs-` followed by this Codespace's runtime UUID |

Scripts can use these values to generate connection instructions without
asking users to enter the address again. An independent shell that does not
read login profiles can load the same environment:

```sh
. /var/lib/gitea-codespace/environment/env.sh
```

This file also contains access credentials; source it rather than printing or
publishing its contents. System services receive the corresponding
`systemd.env` through the platform's service configuration.

The variables describe the Gateway selected for the site when the Runtime Pod
is created. They remain stable for that Pod. After an operator changes the
public address, stop and resume the Codespace to refresh them. The same behavior
applies to Kata, Sysbox, and Native environments. Platform-provided values take
precedence over secrets with the same names so connection instructions identify
the current workspace.

## Other uses

Use the host, port, and user shown by Gitea or the IDE to create a local
`~/.ssh/config` entry. Replace the example values:

```sshconfig
Host my-codespace
    HostName ssh.example.com
    Port 2222
    User cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986
    ServerAliveInterval 30
    ServerAliveCountMax 3
```

Gateway applies its SSH session idle timeout to shells and port-forwarding
sessions alike. Keeping a listener open does not count as activity. The
keepalive requests keep an otherwise idle tunnel active and let the client
detect an unresponsive connection; they do not reconnect a closed session.
Choose an interval shorter than the Gateway's configured SSH idle timeout.

That entry works with ordinary SSH clients and desktop editors supporting
Remote SSH. From your local computer:

| Task | Command |
| --- | --- |
| Open a shell | `ssh my-codespace` |
| Transfer files | `sftp my-codespace` |
| Reach Dev Container port 8080 locally | `ssh -NT -L 127.0.0.1:8080:127.0.0.1:8080 my-codespace` |

For remote USB devices, use **Copy USB tunnel command** in the IDE's USB view
and follow the [USB connection guide](usbip.md#connect-a-source). Copying a
command only prepares instructions; the connection begins when you run it on
the computer that can reach the USB/IP server.

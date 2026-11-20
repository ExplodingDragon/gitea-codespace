# Workspace events

`gitea-codespace-event` sends diagnostic messages from the primary Dev Container
to its Gitea Codespace log. During creation and resume, the platform installs it
after the primary container starts and before container lifecycle commands such
as `onCreateCommand` and `postStartCommand`, and before Web IDE startup.
The image entrypoint and the outer Runtime's `initializeCommand` run earlier.
Ordinary container users, scripts, and providers use the same command and
instance allowance.

## Send an event

```sh
gitea-codespace-event workspace.notice "Development service is ready"
gitea-codespace-event webide.error "Editor process exited with code 1"
```

The command takes exactly two arguments. Quote the message to preserve spaces
and line breaks. Types contain 1 to 48 ASCII letters, digits, dots, underscores,
or hyphens; messages contain 1 to 16384 bytes of UTF-8 text. Each message line
appears with an `[EVENT] [type]` prefix. One command counts as one event even
when its message spans several lines.

Success means Manager accepted the event into its log queue, not that Gitea has
already saved it. Invalid input, rate limits, unavailable services, and full
queues return a nonzero exit status with an explanation. A request lasts at
most five seconds. Keep diagnostic reporting separate from application cleanup
so a reporting error does not prevent the application from stopping.

## Delivery and limits

The default allowance is five accepted events per instance in any one-second
interval. Administrators adjust it through the [Helm event setting](../charts/gitea-codespace/README.md#configure-event-limits).
All event types and callers share that allowance. Rate counters are held in
memory and start fresh when Manager leadership changes.

The platform redacts known instance credentials and associates events with the
sending instance. Events report application observations; they do not change
Codespace state or count as user activity. Avoid including credentials in
diagnostic messages, because arbitrary secrets supplied by applications may be
unknown to the platform.

Events share the existing bounded log queue and Gitea log size limit. Framing
many lines or redacting a message can increase its size; an event that exceeds
the remaining admission budget is rejected as a whole and does not consume the
instance's rate allowance. Accepted events can be lost during process failure,
operation changes, or resource cleanup. After Manager recovers, running instances
can send new events; events not yet delivered to Gitea may have been lost.
This makes the interface suitable for concise diagnostics rather than bulk
application output or durable notifications. Read delivered events in the
Gitea Codespace log.

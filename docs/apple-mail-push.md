# Native Apple Mail push

IMAP IDLE already updates connected clients. Sleeping iPhone Mail needs Apple's
`XAPPLEPUSHSERVICE` extension and APNs. An ordinary app's APNs topic cannot wake
Apple's Mail application. An Apple-approved Mail topic is a prerequisite.

The implementation is optional and disabled by default. It advertises the
extension only after authentication and only when push credentials are configured.
Mail registers an opaque account ID, device token, and subscribed mailboxes.
Registrations and coalesced pending hints live in SQLite. Mailbox changes and
pending hints commit in the same transaction. A worker sends HTTPS/HTTP2 APNs
requests and retries failures; a newer change arriving during delivery remains
pending. APNs receives only the opaque account ID, never mail contents. Invalid
device tokens are removed; authentication/configuration failures retain pending
work for retry. Back up the persistent volume: it contains both mailbox state
and device registrations. Do not log or publish device tokens or private keys.

The IMAP dependency is pinned to a reviewed commit of `denislemire/go-imap`,
which adds the optional session extension. It does not add a second protocol
parser in the gateway. See the fork's PR #1 for its wire parser and tests.

## Obtain a Mail topic

As of October 2026 the old macOS Server push-certificate endpoint is retired.
The maintained XAPS project describes a coordinated request process through
Apple Developer Relations for a dedicated topic linked to a developer account.
A future self-service mechanism is discussed but cannot be assumed available.
Use the current instructions in
[the XAPS provisioning discussion](https://github.com/freswa/dovecot-xaps-daemon/issues/43#issuecomment-5915184382)
and [the deployment guide](https://docker-mailserver.github.io/docker-mailserver/v15.0/examples/use-cases/ios-mail-push-support/).
Verify the current contact/process before sending anything. This repository does
not send requests or share credentials with that project.

Prepared request for Denis to submit through the confirmed provisioning channel:

> I operate a private IMAP service at imap.ehws.generic.business for the
> generic.business domain and would like a dedicated Apple Mail APNs push topic
> linked to my Apple Developer membership. My Team ID is JQENKH9X53.
> The service implements XAPPLEPUSHSERVICE version 2 and uses the production
> APNs provider API. Please advise the current process for provisioning a Mail
> topic for this team, and the required authorization for token-based APNs
> authentication. I can provide membership verification through an official
> Apple channel. I will not send private keys or account passwords.

No topic has been granted and no request has been sent by this implementation.
Do not invent a topic string or use another operator's shared credentials.

## Activate after approval

Confirm the actual Mail topic and that the APNs signing key is authorized for
it. Keep the .p8 private key in the existing secret-management workflow, not Git,
chat, Terraform outputs, or Helm values. Project it as a Kubernetes Secret key
`private-key.p8`. The Helm mount is read-only and accessible to the pod's group.

Set these chart values through the cluster repository:

```yaml
applePush:
  enabled: true
  topic: <approved Mail topic>
  teamID: JQENKH9X53
  keyID: <10-character APNs key ID>
  existingSecret: <secret containing private-key.p8>
```

Equivalent environment variables are `APPLE_PUSH_TOPIC`, `APPLE_PUSH_TEAM_ID`,
`APPLE_PUSH_KEY_ID`, and `APPLE_PUSH_KEY_FILE`. Partial/invalid configuration
fails startup. The signing key is loaded at startup; roll the pod after rotating
it. Production APNs uses HTTPS 443, already allowed by the chart's AWS egress.

Reconnect the iPhone Mail account so it sees the newly advertised capability and
registers. Send a new message with Mail backgrounded and the phone locked.
Confirm delivery and stable read flags on reconnect. Repeat after restarting the
pod. Unit tests cover wire registration, token authentication, HTTP2 payloads,
retries, revoked tokens, persistence, and concurrent changes; they cannot prove
Apple has enabled the team's entitlement or that a real phone wakes.

APNs hints are best-effort and can be throttled or coalesced by Apple. The IMAP
mailbox is authoritative; a push asks Mail to reconnect and synchronize. SQS
makes S3 arrival detection faster but does not remove Apple's provisioning step.
Disable `applePush.enabled` to stop advertising and sending; normal IMAP remains
available and existing registrations stay on the volume.

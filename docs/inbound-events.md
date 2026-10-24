# S3 arrivals through SQS

The optional path is SES → S3 `inbound/` → standard SQS queue → local SQLite
commit → IMAP IDLE updates and, when enabled, Apple Mail push hints. No public
webhook or Redis is needed. Apple credentials are independent of SQS.

Set `SQS_QUEUE_URL` (chart `inboundEvents.queueURL`) and optionally `SQS_REGION`
(`inboundEvents.region`, defaults to the S3 region). The queue must be in the
bucket's region. The AWS credential chain is the same projected service-account
identity used for S3. Grant `sqs:ReceiveMessage` and `sqs:DeleteMessage` on only
that queue. The queue policy grants S3 `sqs:SendMessage`, restricted by bucket
ARN and AWS source account. The bucket notification selects only ObjectCreated
under `inbound/`, avoiding state writes and feedback loops.

The `home` repository owns these resources in Terraform Cloud:
`terraform/generic_mail_events.tf` and `terraform/iam_generic_mail.tf`. Before the
first apply, inspect the bucket's existing notifications. S3 replaces the whole
notification configuration, so existing rules must be represented and preserved.
Do not apply until that inspection and the Terraform Cloud plan are reviewed.
Use the Terraform output `generic_mail_inbound_queue_url` in the cluster's Flux
HelmRelease after AWS has created the queue. No bucket retention policy is changed.

## How delivery works

A receive waits up to 20 seconds, returning as soon as mail arrives. Receiving a
notification hides it for 120 seconds; it does not delete it. The gateway gets
the named S3 object and commits its UID and mailbox state before acknowledging
with DeleteMessage. A crash before acknowledgement causes redelivery. A crash
after commit is safe because the durable source-key index deduplicates retries.
S3/SQS can deliver duplicates or deliver out of order; neither arrival order nor
an SQS message ID is used as an IMAP UID.

One notification is processed at a time, with a 60-second deadline per object.
Queue retention is four days. Five failed deliveries move a notification to the
dead-letter queue, retained for 14 days. Inspect failures before redriving; a
large or inaccessible S3 object will keep failing until its cause is resolved.
Malformed or unexpected notifications are not silently acknowledged. Queue
bodies and receipt handles are not logged. Watch the dead-letter queue's visible
message count in AWS; no alert integration is provisioned by this change.

A startup scan and periodic reconciliation remain. With SQS configured, the
application's default scan interval is five minutes, versus 30 seconds without
it. `SYNC_INTERVAL` / `mailbox.syncInterval` overrides that default. An existing
explicit `30s` Helm override must be removed or changed to reduce listing calls.
Reconciliation recovers arrivals during configuration changes or queue expiry.
It also retries pending S3 deletions. It imports missed objects but does not
remove mailbox entries just because an object disappeared outside the gateway.

## Verify rollout

1. Apply the reviewed Terraform Cloud run and confirm the queue URL output.
2. Configure that URL through Flux, keeping Apple push disabled until provisioned.
3. Send a test message to the existing SES address. Verify one new IMAP UID
   appears promptly, queue depth drains, and the dead-letter queue stays empty.
4. Restart the gateway and confirm the message and its read flags remain stable.
5. Confirm reconciliation still works and that no duplicate UID appears.

To revert, unset the queue URL and restore the 30-second scan interval. Leave
queue resources in place until any outstanding events have been investigated.

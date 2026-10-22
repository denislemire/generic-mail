# generic-mail

One process. It serves IMAP for objects already in a bucket, and authenticated SMTP that hands the raw message to Amazon SES.

INBOX is the inbound prefix. This process does not delete those objects. Sent and Drafts are prefixes it writes. Trash is an index view. Flags and UIDs live in `imap-state/mailboxes.json`. Deleting that object changes UID validity and forces clients to resync.

Message bodies are cached in the process (64 MiB). The first fetch reads the bucket. Later fetches in that process do not.

SMTP is AUTH PLAIN after TLS. Ports are IMAPS 993, submission 587, and SMTPS 465. The AWS SDK uses its default credential chain, including a projected service-account token when `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` are set.

Required environment: `MAIL_USERNAME`, `MAIL_PASSWORD`, `MAIL_HOSTNAME`, `S3_BUCKET`.

The chart defaults are generic. A cluster sets the bucket, hostname, role, and image in its own values.

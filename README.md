# generic-mail

One process. It serves IMAP for objects already in a bucket, and authenticated SMTP that hands the raw message to Amazon SES.

INBOX is the inbound prefix. Those objects are never deleted or rewritten. On an empty local database the process imports `imap-state/mailboxes.json` once, including UIDs, UID validity, flags, and skipped keys, and does not write that object back. After that, flags, folders, and UIDs live in SQLite on `MAIL_DATA` (default `/var/lib/generic-mail`). Copying or appending into any mailbox other than INBOX writes the body as a file on that volume. Losing the volume drops local state and forces clients to resync; the inbound objects remain in the bucket.

Message bodies are cached in the process (64 MiB). The first fetch reads the bucket or the local file. Later fetches in that process do not.

SMTP is AUTH PLAIN after TLS. Ports are IMAPS 993, submission 587, and SMTPS 465. The AWS SDK uses its default credential chain, including a projected service-account token when `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` are set.

When `OAUTH_JWKS_URL` is set, IMAP and submission also advertise `XOAUTH2` and `OAUTHBEARER`. The initial response is the one Roundcube sends: the SASL username is an allowed identity, and `auth=` is an RS256 access token (a `Bearer ` prefix is optional). The token issuer, audience, and either `preferred_username` or `email` must match `OAUTH_ISSUER`, `OAUTH_AUDIENCE`, and `OAUTH_ALLOWED`. Password login is unchanged.

Required environment: `MAIL_USERNAME`, `MAIL_PASSWORD`, `MAIL_HOSTNAME`, `S3_BUCKET`. `MAIL_DATA` is the writable directory for the SQLite index and local message files.

The chart defaults are generic. A cluster sets the bucket, hostname, role, and image in its own values.

# Incremental IMAP synchronization and keyword labels

Release 0.7 implements RFC 7162 CONDSTORE and QRESYNC. Each mailbox has a
persistent, increasing 63-bit change number (HIGHESTMODSEQ); each message has
its last flag-change number (MODSEQ). Arrivals, APPEND, COPY, flag changes,
and expunges advance that mailbox's clock. Reapplying existing flags does not.

Change numbers and UID deletion records commit in the same SQLite transaction
as message state. Notifications publish only after commit. Conditional STORE
compares and changes flags under the same lock and transaction, so two stale
writers cannot both win. Partial success returns tagged OK [MODIFIED ids] for
conflicts while retaining successfully committed changes.

Supported operations:

- SELECT/EXAMINE (CONDSTORE), ENABLE CONDSTORE and ENABLE QRESYNC.
- STATUS HIGHESTMODSEQ; FETCH MODSEQ and CHANGEDSINCE, including zero.
- STORE UNCHANGEDSINCE, including zero and .SILENT responses.
- SEARCH MODSEQ, including nested boolean expressions and flag entry names.
  A shared message-level clock is used for flag entries rather than separate
  per-flag clocks; changes to another flag can conservatively produce a conflict.
- SELECT/EXAMINE QRESYNC with UIDVALIDITY, last modseq, optional known UIDs,
  and optional sequence-match hints. The hints are parsed; full retained history
  supplies the result. Known UID filters restrict changed and vanished results.
- UID FETCH CHANGEDSINCE VANISHED; VANISHED (EARLIER) precedes changed FETCHes.
- Enabled sessions get VANISHED for live expunges; other sessions keep EXPUNGE.
  Pending deletions retain legacy sequence mappings until delivered. A lower
  HIGHESTMODSEQ prevents a client from caching past a deferred deletion.

Existing databases receive a non-zero baseline without changing UIDs or
UIDVALIDITY. History predating that baseline is recovered as absent UID ranges
when CHANGEDSINCE 0 is requested. Tombstones are retained for the mailbox's
lifetime (no pruning yet); deleting the mailbox removes its history.

Synchronization data uses one additive SQLite table. Fingerprints detect base
mailbox changes made by older binaries during rollback and rebase current
messages/deletions above the persisted clock on the next upgrade. Do not restore
an older backup into an active mailbox identity without a UIDVALIDITY reset:
clients may already have observed state newer than that backup.

## Labels foundation

IMAP keywords are persisted with flags, survive COPY and restart, and are
searchable with KEYWORD/UNKEYWORD without reading bodies. SELECT advertises
available keywords and PERMANENTFLAGS includes `\*`; selected sessions receive
updated FLAGS inventories. Names are case-insensitive IMAP atoms (no spaces);
the engine currently canonicalizes them to lowercase. A future web interface
can map stable keywords to friendly display names and colors.

These are tags on messages within mailboxes, not Gmail's global label model.
There is no global message identity, label catalog, virtual label mailbox, or
cross-folder deduplication API yet. None is advertised as supported.

## Roundcube

After verifying the capabilities on the deployed gateway, set `messages_cache`
to true alongside `imap_cache = 'db'`. This uses Roundcube's existing SQLite
database and needs no Redis. Recreate the webmail pod through its Git-managed
configuration revision because custom config is mounted using subPath. Log in
again so cached server capabilities are refreshed.

Verify with two clients, synthetic messages, changed flags, UID-scoped expunge,
and a reconnect against a restarted gateway. Never use real messages for smoke
mutations. Keep HTML rendering and future webmail work separate from this change.

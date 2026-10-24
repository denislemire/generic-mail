# Local-first mailbox reads and search

The gateway now uses local resources for interactive work:

- UID lists, counts, flags, dates and sizes come from the mailbox index without
  reading message content. Boolean metadata searches short-circuit correctly.
- Body/header/envelope requests use RAM, then persistent disk, then S3. Cold
  concurrent requests share a bounded set of load locks rather than downloading
  the same object repeatedly.
- Positive substring searches use a SQLite FTS5 trigram index to narrow possible
  matches. Each candidate is still checked using the existing IMAP matcher.
  Search terms are quoted literals, never SQL or FTS operators from user input.
- Missing/incomplete index entries remain candidates. Short, non-ASCII, NOT and
  mixed boolean searches safely fall back to local scanning where necessary.
  Index errors also fall back to exact scanning. The index accelerates existing
  search semantics; it does not add PDF/OCR or attachment-content extraction.

This uses the existing SQLite library, no Redis or separate search service.
The [SQLite trigram tokenizer](https://www.sqlite.org/fts5.html#the_trigram_tokenizer)
supports substrings rather than requiring whole-word matches. This matters for
IMAP, where replacing substring matching with ordinary word search would change
results.

## Storage, backfill and limits

Owned APPEND/COPY messages remain in `MAIL_DATA/files`; that is primary storage.
Disposable S3 bodies live in `MAIL_DATA/cache/s3`, named by hashes instead of S3
keys. Writes use private temporary files and atomic rename. The cache discovers
existing bodies at startup and enforces an LRU byte budget; it never deletes
owned mail files to make space. Cache entries larger than the configured budget
are served without persisting them. Retiring a Trash source also removes its
cached body, with retry if local cleanup fails.

`MAIL_DISK_CACHE_BYTES` / Helm `cache.diskBytes` defaults to 536870912 (512 MiB).
Zero disables disk body caching. The existing 64 MiB RAM cache remains bounded.
Provision room for owned mail, SQLite/WAL/search indexes and the disposable
cache. The chart uses a 512 MiB memory limit to allow bounded backfill alongside
large messages. Space is a local operational requirement; disk-cache/index write
failures leave S3 fallback available and do not reject a committed delivery.

Startup backfill processes referenced messages with four workers. It indexes
missing documents and fills disk cache where budget permits. Existing local
content survives restarts. With more mail than fits the disk budget, older
uncached bodies can still require S3 reads; this is a bounded cache rather than
an unlimited second archive. First upgrade backfill downloads existing content
once. New S3 arrivals seed derived content after mailbox commit, before the
consumer acknowledges its notification. APPEND seeds its index after commit;
COPY projections are built lazily when read or during subsequent backfill.

FTS records are indexed once per immutable location/key. Bodies above 4 MiB are
marked for exact scanning rather than generating unbounded trigram indexes.
Candidate selection never excludes these records. Flags and folder moves do not
re-index unchanged bytes. Removing the last mailbox reference removes its search
projection in the same transaction. The implementation assumes inbound SES
object keys are immutable, matching the mailbox source-key identity model.

Primary mailbox rows, flags, UID validity, pending deletes and push registrations
remain authoritative. Do not remove `state.db` or `files` as cache maintenance.
The disk-cache directory is disposable; missing entries refill from S3. Search
projections are rebuildable, and an interrupted backfill is safe to resume. A
binary rollback can leave unused derived tables/files; they do not change old
UIDs or folder state.

## Measure

The private health port now serves Prometheus-format `/metrics`, with body-load
counters by source (`memory`, `disk`, `s3`) and disk-cache bytes/entries. It emits
no account names, message IDs, S3 keys or contents. Read counters exclude S3
fetches used to ingest new delivery; that AWS work remains expected.

After backfill, compare counters before and after listing, header fetching and
searching, then after a pod restart. For a working set within the disk budget,
interactive operations should not increase the S3 read counter. Startup logs
summarize checked/cached/indexed counts without message identifiers.

A synthetic 700-message benchmark on the development machine measured roughly
8.1 ms for scanning versus 0.95 ms for indexed candidates plus exact matching,
with approximately 7.2 MiB versus 73 KiB of allocations per search. These are
local CPU measurements, not a promised browser load time. Repeat via:

```sh
go test ./internal/mailbox -run '^$' -bench BenchmarkSearchIndex -benchtime=250ms
```

Roundcube can additionally use its SQL-backed `imap_cache` for indexes. Its
`messages_cache` option requires QRESYNC/CONDSTORE; keep that disabled while the
gateway does not implement those extensions. No capability should be advertised
just to enable a client cache.

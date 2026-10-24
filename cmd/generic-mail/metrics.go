package main

import (
	"fmt"
	"net/http"

	"github.com/denislemire/generic-mail/internal/mailbox"
)

func writeContentMetrics(w http.ResponseWriter, stats mailbox.ContentStats) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	const format = `# HELP generic_mail_read_body_loads_total Read-path body loads; excludes incoming S3 delivery reads.
# TYPE generic_mail_read_body_loads_total counter
generic_mail_read_body_loads_total{source="memory"} %d
generic_mail_read_body_loads_total{source="disk"} %d
generic_mail_read_body_loads_total{source="s3"} %d
# TYPE generic_mail_disk_cache_bytes gauge
generic_mail_disk_cache_bytes %d
# TYPE generic_mail_disk_cache_entries gauge
generic_mail_disk_cache_entries %d
`
	_, _ = fmt.Fprintf(w, format, stats.MemoryHits, stats.DiskHits, stats.S3Reads, stats.DiskBytes, stats.DiskEntries)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #467 — the streaming export. These are pure unit tests over an
// injected PageFetcher, so no DB is needed: the streaming invariant and
// the IP-redaction rule are both properties of StreamExport itself.

package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// countingFetcher yields `total` synthetic rows in pages, counting how
// many pages were actually pulled. It is the instrument for the
// streaming test: if the exporter buffered, it would pull every page
// before the reader is touched.
type countingFetcher struct {
	total  int
	served int
	pages  int64
}

func (f *countingFetcher) fetch(_ context.Context, cur ExportCursor, limit int) ([]ExportAuditEventsPageRow, error) {
	atomic.AddInt64(&f.pages, 1)
	if f.served >= f.total {
		return nil, nil
	}
	n := limit
	if f.served+n > f.total {
		n = f.total - f.served
	}
	rows := make([]ExportAuditEventsPageRow, n)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < n; i++ {
		idx := f.served + i
		rows[i] = ExportAuditEventsPageRow{
			ID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
			EventType:  "login.succeeded",
			OccurredAt: pgtype.Timestamptz{Time: base.Add(time.Duration(idx) * time.Second), Valid: true},
		}
	}
	f.served += n
	return rows, nil
}

// TestExport_Streams_DoesNotBufferAllRows is acceptance 4. With far
// more rows than one page, reading only the first page's worth of bytes
// must have pulled only a bounded number of pages from the source — not
// all of them. A buffering implementation fails this because it would
// drain the fetcher fully before returning any bytes.
func TestExport_Streams_DoesNotBufferAllRows(t *testing.T) {
	const total = ExportPageSize * 50 // 50 pages of rows
	f := &countingFetcher{total: total}

	body := StreamExport(context.Background(), FormatCSV,
		SelectColumns("id,event_type"), false, f.fetch)
	defer body.Close()

	// Read just the first page-worth of rows and stop.
	br := bufio.NewReader(body)
	lines := 0
	for lines < ExportPageSize/2 {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("read: %v", err)
		}
		lines++
	}

	pages := atomic.LoadInt64(&f.pages)
	// Reading half a page can only have required the first page (plus at
	// most one lookahead). If the whole 50-page set was pulled, the
	// exporter buffered.
	if pages > 3 {
		t.Errorf("reading half a page pulled %d source pages; the export is buffering, "+
			"not streaming — a 10M-row export would OOM", pages)
	}
	if pages >= int64(total/ExportPageSize) {
		t.Errorf("the exporter drained every page (%d) before the reader finished; not streaming", pages)
	}
}

// TestExport_FullRoundTrip confirms a complete read yields every row and
// terminates (the short-page condition works).
func TestExport_FullRoundTrip(t *testing.T) {
	const total = ExportPageSize*2 + 7 // not a page multiple
	f := &countingFetcher{total: total}
	body := StreamExport(context.Background(), FormatNDJSON, SelectColumns(""), true, f.fetch)
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1
	if lines != total {
		t.Errorf("ndjson lines=%d, want %d", lines, total)
	}
}

// rowWithIP builds a single-row fetcher carrying an IP.
func rowWithIP(ip string) PageFetcher {
	served := false
	return func(_ context.Context, _ ExportCursor, _ int) ([]ExportAuditEventsPageRow, error) {
		if served {
			return nil, nil
		}
		served = true
		addr := netip.MustParseAddr(ip)
		return []ExportAuditEventsPageRow{{
			ID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
			EventType:  "login.succeeded",
			OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			Ip:         &addr,
		}}, nil
	}
}

// TestExport_RedactsIPWithoutPIICapability is acceptance 5, CSV side.
// The exporter must withhold the ip exactly as the list view does.
func TestExport_RedactsIPWithoutPIICapability(t *testing.T) {
	const ip = "203.0.113.7"
	cols := SelectColumns("event_type,ip")

	// includeIP=false → the ip cell is blank.
	redacted := readAll(t, StreamExport(context.Background(), FormatCSV, cols, false, rowWithIP(ip)))
	if strings.Contains(redacted, ip) {
		t.Errorf("export leaked the IP to a caller without system.audit.pii.read:\n%s", redacted)
	}

	// includeIP=true → the ip is present, proving the capability grants
	// something and this isn't just a broken column.
	full := readAll(t, StreamExport(context.Background(), FormatCSV, cols, true, rowWithIP(ip)))
	if !strings.Contains(full, ip) {
		t.Errorf("export withheld the IP from a caller WITH system.audit.pii.read:\n%s", full)
	}
}

// TestExport_RedactsIPInNDJSON covers the JSON path too.
func TestExport_RedactsIPInNDJSON(t *testing.T) {
	const ip = "203.0.113.9"
	cols := SelectColumns("event_type,ip")

	redacted := readAll(t, StreamExport(context.Background(), FormatNDJSON, cols, false, rowWithIP(ip)))
	var obj map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(redacted)), &obj)
	if obj["ip"] != nil {
		t.Errorf("ndjson export leaked ip=%v to a non-PII caller", obj["ip"])
	}
}

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

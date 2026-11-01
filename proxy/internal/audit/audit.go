package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/x/gormdb"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/auditid"
)

const tracerName = "github.com/discobox-ai/discobox/proxy"

// HTTPEvent is an asynchronous audit event for an HTTP exchange.
type HTTPEvent struct {
	Context              context.Context `gorm:"-"`
	Time                 time.Time
	EnqueuedAt           time.Time
	ClientID             string
	ClientSubject        string
	ClientSerial         string
	Method               string
	URL                  string
	Host                 string
	Status               int
	Duration             time.Duration
	Blocked              bool
	BlockedReason        string
	CacheHit             bool
	CacheStored          bool
	CacheKey             string
	CacheError           string
	AppliedRuleID        string
	AppliedPattern       string
	AppliedHeaders       []string
	ForwardedURL         string
	SwappedSecretIDs     []string
	RawBindingIDs        []string
	RedactRequestHeaders []string
	RequestHeaders       http.Header
	ResponseHeaders      http.Header
	ResponseBytes        int64
	RequestBodyFile      string
	RequestBodyFormat    string
	RequestBodyBytes     int64
	RequestBodyError     string
	ResponseBodyFile     string
	ResponseBodyFormat   string
	ResponseBodyBytes    int64
	ResponseBodyError    string
	Upgrade              bool
	UpgradeType          string
	UpgradeC2SBytes      int64
	UpgradeS2CBytes      int64
	StreamSessionID      string
	StreamFile           string
	StreamFormat         string
	StreamDroppedChunks  uint64
	StreamDroppedBytes   uint64
	// Tunnel is a passthrough CONNECT's record (ADR 26-10-31-819 §4); zero
	// on every other row.
	Tunnel Tunnel
}

// A tunnel row's phase (Tunnel.Phase).
const (
	TunnelPhaseOpen   = "open"
	TunnelPhaseClose  = "close"
	TunnelPhaseFailed = "failed"
)

// Tunnel is what the proxy sees of a CONNECT it splices unread: everything
// outside the tunnel, nothing inside it. A tunnel is two rows sharing ID, an
// open row and a close row, because a row is never updated once written:
// followers read forward by row ID and would never see the update. A dial
// that fails is one row, Phase "failed".
type Tunnel struct {
	// ID is shared by a tunnel's rows.
	ID string `gorm:"index"`
	// Phase is TunnelPhaseOpen, TunnelPhaseClose, or TunnelPhaseFailed;
	// empty on a row that is no passthrough tunnel's.
	Phase string
	// Entry is the passthrough entry the CONNECT's host matched.
	Entry string
	// DialedAddress is the address the proxy connected to: the origin's, or
	// the upstream proxy's when the tunnel went through one.
	DialedAddress string
	// UpstreamProxy is the upstream proxy the tunnel went through, redacted;
	// empty when it went direct.
	UpstreamProxy string
	// TLS is whether the client's first flight was a TLS ClientHello, and
	// ServerName, ALPN and TLSVersions what it offered, comma-joined.
	// ServerNameMismatch is a ServerName naming a host other than the
	// CONNECT's.
	TLS                bool
	ServerName         string
	ALPN               string
	TLSVersions        string
	ServerNameMismatch bool
	// C2SBytes and S2CBytes are the bytes carried each way, on the close row.
	C2SBytes int64
	S2CBytes int64
	// ClosedBy is the side whose stream ended first, "client" or "server",
	// on the close row.
	ClosedBy string
	// Error is why the dial failed, or the error that ended the tunnel.
	Error string
}

// SOCKSEvent is an asynchronous audit event for a SOCKS connect attempt.
type SOCKSEvent struct {
	Context       context.Context `gorm:"-"`
	Time          time.Time
	EnqueuedAt    time.Time
	ClientID      string
	ClientSubject string
	ClientSerial  string
	Destination   string
	Port          int
	Allowed       bool
	BlockedReason string
}

// DNSEvent is an asynchronous audit event for one DNS query a sandbox asked
// of its pool (ADR 0148). Name and Type are the question; RCode and Answers
// what came back, empty when nothing did, and Error why not.
type DNSEvent struct {
	Context       context.Context `gorm:"-"`
	Time          time.Time
	EnqueuedAt    time.Time
	ClientID      string
	ClientSubject string
	ClientSerial  string
	Name          string
	Type          string
	RCode         string
	Answers       []string
	Duration      time.Duration
	Error         string
}

// HTTPExchange is the GORM model for audited HTTP exchanges.
type HTTPExchange struct {
	// ID is the row number, and on every API above this database it is written
	// as http_<number> (auditid.ExchangeID): the number is the write order a
	// cursor reads along, and the prefix is what tells a reader which trail an
	// ID names.
	ID             auditid.ExchangeID `gorm:"primaryKey"`
	CreatedAt      time.Time
	EnqueuedAt     time.Time
	WrittenAt      time.Time
	ClientID       string `gorm:"index"`
	ClientSubject  string
	ClientSerial   string
	Method         string
	URL            string
	Host           string `gorm:"index"`
	Status         int
	DurationMillis int64
	DurationMicros int64
	Blocked        bool
	BlockedReason  string
	CacheHit       bool
	CacheStored    bool
	CacheKey       string
	CacheError     string
	AppliedRuleID  string
	AppliedPattern string
	AppliedHeaders string
	// ForwardedURL is where a gateway sent the request (ADR 26-10-16-470 §3);
	// URL stays what the sandbox sent. AppliedRuleID is the gateway's name,
	// AppliedPattern its match, and AppliedHeaders the headers it touched.
	ForwardedURL string
	// SwappedSecretIDs names the secrets whose values this request carried,
	// comma-joined (ADR 26-10-01-240). It never holds a sentinel: this trail
	// is kept to be read later. Unindexed: nothing queries it.
	//
	// AutoMigrate adds it to a database written before it, where those rows
	// read it as empty: nothing was recorded about their secrets.
	SwappedSecretIDs string
	// RawBindingIDs names the raw bindings whose values this request was
	// found carrying and let through on their routes, comma-joined (ADR
	// 26-10-27-840 §5); each one's secret is in SwappedSecretIDs too. Like
	// it, never a value and unindexed, and AutoMigrate adds it to a database
	// written before it, whose rows read it as empty.
	RawBindingIDs       string
	RequestHeaders      string
	ResponseHeaders     string
	ResponseBytes       int64
	RequestBodyFile     string
	RequestBodyFormat   string
	RequestBodyBytes    int64
	RequestBodyError    string
	ResponseBodyFile    string
	ResponseBodyFormat  string
	ResponseBodyBytes   int64
	ResponseBodyError   string
	Upgrade             bool
	UpgradeType         string
	UpgradeC2SBytes     int64
	UpgradeS2CBytes     int64
	StreamSessionID     string
	StreamFile          string
	StreamFormat        string
	StreamDroppedChunks uint64
	StreamDroppedBytes  uint64
	// Tunnel is a passthrough tunnel's record. AutoMigrate adds its columns
	// to a database written before it, whose rows read them as empty.
	Tunnel Tunnel `gorm:"embedded;embeddedPrefix:tunnel_"`
}

// SOCKSConnect is the GORM model for audited SOCKS connections.
type SOCKSConnect struct {
	ID            uint `gorm:"primaryKey"`
	CreatedAt     time.Time
	EnqueuedAt    time.Time
	WrittenAt     time.Time
	ClientID      string `gorm:"index"`
	ClientSubject string
	ClientSerial  string
	Destination   string `gorm:"index"`
	Port          int
	Allowed       bool
	BlockedReason string
}

// DNSQuery is the GORM model for audited DNS queries.
type DNSQuery struct {
	// ID is the row number, written as dns_<number> (auditid.DNSQueryID) on
	// every API above this database, for the reason HTTPExchange's is.
	ID auditid.DNSQueryID `gorm:"primaryKey"`
	// CreatedAt is indexed, unlike the smaller tables': this is the trail
	// with the most rows, and both a read by time and the retention sweep
	// would otherwise scan all of it.
	CreatedAt     time.Time `gorm:"index"`
	EnqueuedAt    time.Time
	WrittenAt     time.Time
	ClientID      string `gorm:"index"`
	ClientSubject string
	ClientSerial  string
	// Name is the name asked, lowercased and without the root's trailing dot:
	// the form a reader filters by.
	Name  string `gorm:"index"`
	Type  string
	RCode string
	// Answers is the answer section's data — addresses, and the targets of
	// CNAMEs and the like — as a comma-joined list.
	Answers        string
	DurationMillis int64
	DurationMicros int64
	Error          string
}

// Recorder asynchronously persists audit events.
type Recorder struct {
	enabled bool
	db      *gorm.DB
	ch      chan any
	// dnsCh holds DNS events apart from ch, and the writer takes from it only
	// when ch is empty: a sandbox's lookups can far outnumber its requests,
	// and on one queue a flood of them would push the HTTP and SOCKS rows out
	// (ADR 0148). dnsLimits bounds each sandbox's share of it.
	dnsCh           chan any
	dnsDropped      atomic.Uint64
	dnsMu           sync.Mutex
	dnsLimits       map[string]*rate.Limiter
	done            chan struct{}
	enqueueMu       sync.RWMutex
	wg              sync.WaitGroup
	streamDir       string
	bodyDir         string
	streamQueueSize int
	streamWg        sync.WaitGroup
	dropped         atomic.Uint64
	closed          atomic.Bool
	// openSpool names the spool files currently being written, so the
	// retention sweep does not reclaim one out from under a live stream.
	spoolMu   sync.Mutex
	openSpool map[string]struct{}
	// budget bounds the spool trees by bytes (ADR 26-10-08-698). spoolBytes
	// is their running total, spoolLimit the budget the last pass resolved,
	// and overBudget wakes a pass when writes take the total past it.
	budget     SpoolBudget
	spoolBytes atomic.Int64
	spoolLimit atomic.Int64
	overBudget chan struct{}
	// redactor is what each client's trail is kept clean of (SetRedactor).
	redactor atomic.Pointer[func(clientID string) Redactor]
	// pools owns the database handle db borrows. Close releases it: without
	// that the connection outlives the recorder, which on Linux merely leaks
	// and on Windows makes the database file undeletable for the life of the
	// process.
	pools *gormdb.Pools
}

// QueryOptions filters audit reads. Limit defaults to 100 and is capped at 1000.
type QueryOptions struct {
	ClientID string
	// Host keeps one destination. For HTTP, a host named without a port also
	// keeps that host at any port: a CONNECT the policy refused is recorded
	// by its authority, host:443, and a reader asking about a host means it.
	Host string
	// Since keeps rows written at or after it. Rows are written in UTC
	// (nonZeroTime), and SQLite compares times as text carrying their offset,
	// so the bound is compared in UTC too.
	Since time.Time
	// Until keeps rows written at or before it, compared in UTC as Since is.
	// It is how a reader pages back through history newest first: each page
	// is read up to the oldest row of the one before it, inclusively, so rows
	// sharing that instant are read again rather than skipped.
	Until time.Time
	// MinStatus and MaxStatus bound the response status, inclusive; zero leaves
	// that side open. HTTP reads only.
	MinStatus int
	MaxStatus int
	// Blocked selects exchanges the destination policy refused when true and
	// ones it let through when false; nil is both. HTTP reads only.
	Blocked *bool
	// Ascending returns the oldest rows first. A follower reads forward from a
	// since bound, and newest-first with a limit would drop whatever falls
	// between its last read and the newest page.
	Ascending bool
	// AfterID keeps rows written after the one a reader last read, and orders
	// by id rather than time. It is the cursor a follower should use where it
	// can: the id is the write order, which is the order rows become readable,
	// while created_at is the order they happened. The recorder stamps an
	// exchange when it ends and writes it from a queue, so a slow write lands
	// behind a faster one — and a reader paging by time either re-reads a
	// window on every poll to catch those, or loses them. Reading by id needs
	// neither.
	//
	// It takes precedence over Since, Until and Ascending: with a cursor there
	// is nothing for a time bound to add, and the write order is forward by
	// definition.
	AfterID auditid.ExchangeID
	Limit   int
}

// DNSQueryOptions filters DNS audit reads. Its fields mean what QueryOptions'
// do, AfterID included; Name is the name asked, matched exactly.
type DNSQueryOptions struct {
	// ID reads the one row with this ID, which is how a reader handed one
	// record reads it: the list is the only read this trail has.
	ID        auditid.DNSQueryID
	ClientID  string
	Name      string
	Since     time.Time
	Until     time.Time
	Ascending bool
	AfterID   auditid.DNSQueryID
	Limit     int
}

// order is the ORDER BY for a read positioned by a row cursor (after) or read
// oldest first (ascending). id breaks ties between rows written in the same
// instant, so a reader paging forward sees a stable sequence.
func order(after uint64, ascending bool) string {
	if after > 0 {
		// Write order, which is what a cursor reads along. It is also the
		// primary key, so this is the one read here that needs no index.
		return "id ASC"
	}
	if ascending {
		return "created_at ASC, id ASC"
	}
	return "created_at DESC, id DESC"
}

// ConfigureStreamSpool sets the directory used for raw upgraded-stream spool files.
func (r *Recorder) ConfigureStreamSpool(dir string, queueSize int) {
	if r == nil {
		return
	}
	r.streamDir = dir
	r.streamQueueSize = queueSize
}

// ConfigureBodySpool sets the directory used for raw HTTP body spool files.
func (r *Recorder) ConfigureBodySpool(dir string) {
	if r == nil {
		return
	}
	r.bodyDir = dir
}

// BeginUpgradeStream starts asynchronous raw upgraded-stream spooling.
func (r *Recorder) BeginUpgradeStream(clientID, upgradeType, extensions string) (*StreamRecord, *StreamSession, error) {
	if r == nil || !r.enabled || r.streamDir == "" {
		return nil, nil, nil
	}
	record, session, err := beginUpgradeStream(r.streamDir, clientID, upgradeType, extensions, r.streamQueueSize, &r.streamWg)
	if err != nil || record == nil {
		return record, session, err
	}
	if redactor := r.redactorFor(clientID); redactor != nil {
		session.patterns = redactor.Patterns()
	}
	session.onClose = r.trackSpool(record.File)
	session.onWrite = r.addSpoolBytes
	go session.run()
	return record, session, nil
}

// BeginBody starts a raw HTTP body spool file.
func (r *Recorder) BeginBody(clientID, kind string) (*BodyRecord, *BodySpool, error) {
	if r == nil || !r.enabled || r.bodyDir == "" {
		return nil, nil, nil
	}
	record, spool, err := BeginBody(r.bodyDir, clientID, kind)
	if err != nil || record == nil {
		return record, spool, err
	}
	spool.redaction = newSpoolRedaction(r.redactorFor(clientID))
	spool.onClose = r.trackSpool(record.File)
	spool.onWrite = r.addSpoolBytes
	return record, spool, nil
}

// Open creates the audit store and starts its background writer.
func Open(ctx context.Context, dsn string, queueSize int, enabled bool) (*Recorder, error) {
	if !enabled {
		return &Recorder{enabled: false}, nil
	}
	if queueSize <= 0 {
		queueSize = 16384
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		return nil, err
	}
	if err := pools.Write.WithContext(ctx).AutoMigrate(&HTTPExchange{}, &SOCKSConnect{}, &DNSQuery{}); err != nil {
		_ = pools.Close()
		return nil, err
	}
	if err := dropRetiredColumns(pools.Write.WithContext(ctx)); err != nil {
		_ = pools.Close()
		return nil, err
	}
	r := &Recorder{
		enabled:    true,
		overBudget: make(chan struct{}, 1),
		db:         pools.Write,
		pools:      pools,
		ch:         make(chan any, queueSize),
		dnsCh:      make(chan any, queueSize),
		done:       make(chan struct{}),
	}
	r.wg.Add(1)
	go r.run()
	return r, nil
}

// RecordHTTP queues an HTTP audit event without blocking on SQLite.
func (r *Recorder) RecordHTTP(event HTTPEvent) {
	if event.EnqueuedAt.IsZero() {
		event.EnqueuedAt = time.Now().UTC()
	}
	r.enqueue(redactHTTP(event, r.redactorFor(event.ClientID)))
}

// DNS audit budget per sandbox: a sustained rate and a burst above it. Past
// it a sandbox's own lookups are dropped from the trail and counted, rather
// than spending the queue every sandbox's lookups share.
const (
	dnsAuditRate  = 50
	dnsAuditBurst = 500
)

// RecordDNS queues a DNS audit event without blocking on SQLite. A sandbox's
// lookups are the busiest trail there is, which is exactly why this must never
// wait: they have a queue of their own, written only when the HTTP and SOCKS
// queue is empty, and each sandbox a budget in it. An event over either is
// dropped and counted.
func (r *Recorder) RecordDNS(event DNSEvent) {
	if r == nil || !r.enabled {
		return
	}
	if event.EnqueuedAt.IsZero() {
		event.EnqueuedAt = time.Now().UTC()
	}
	if !r.dnsLimit(event.ClientID).Allow() {
		r.dnsDropped.Add(1)
		return
	}
	r.enqueueOn(r.dnsCh, &r.dnsDropped, event)
}

func (r *Recorder) dnsLimit(clientID string) *rate.Limiter {
	r.dnsMu.Lock()
	defer r.dnsMu.Unlock()
	if r.dnsLimits == nil {
		r.dnsLimits = map[string]*rate.Limiter{}
	}
	limit, ok := r.dnsLimits[clientID]
	if !ok {
		limit = rate.NewLimiter(dnsAuditRate, dnsAuditBurst)
		r.dnsLimits[clientID] = limit
	}
	return limit
}

// RecordSOCKS queues a SOCKS audit event without blocking on SQLite.
func (r *Recorder) RecordSOCKS(event SOCKSEvent) {
	if event.EnqueuedAt.IsZero() {
		event.EnqueuedAt = time.Now().UTC()
	}
	r.enqueue(event)
}

// Dropped returns the number of HTTP and SOCKS events dropped due to
// backpressure.
func (r *Recorder) Dropped() uint64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// DNSDropped returns the number of DNS events dropped, over a sandbox's
// budget or a full DNS queue.
func (r *Recorder) DNSDropped() uint64 {
	if r == nil {
		return 0
	}
	return r.dnsDropped.Load()
}

// ListHTTP returns recent HTTP audit exchanges newest first.
func (r *Recorder) ListHTTP(ctx context.Context, opts QueryOptions) ([]HTTPExchange, error) {
	if r == nil || !r.enabled {
		return nil, nil
	}
	var rows []HTTPExchange
	query := applyHTTPQueryOptions(r.db.WithContext(contextOrBackground(ctx)).Model(&HTTPExchange{}), opts)
	if err := query.Order(order(uint64(opts.AfterID), opts.Ascending)).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListSOCKS returns recent SOCKS connect audit rows newest first.
func (r *Recorder) ListSOCKS(ctx context.Context, opts QueryOptions) ([]SOCKSConnect, error) {
	if r == nil || !r.enabled {
		return nil, nil
	}
	var rows []SOCKSConnect
	query := applySOCKSQueryOptions(r.db.WithContext(contextOrBackground(ctx)).Model(&SOCKSConnect{}), opts)
	if err := query.Order(order(uint64(opts.AfterID), opts.Ascending)).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListDNS returns DNS audit rows, newest first unless opts say otherwise.
func (r *Recorder) ListDNS(ctx context.Context, opts DNSQueryOptions) ([]DNSQuery, error) {
	if r == nil || !r.enabled {
		return nil, nil
	}
	var rows []DNSQuery
	query := applyCursor(r.db.WithContext(contextOrBackground(ctx)).Model(&DNSQuery{}), uint64(opts.AfterID), opts.Since, opts.Until)
	if opts.ClientID != "" {
		query = query.Where("client_id = ?", opts.ClientID)
	}
	if opts.Name != "" {
		query = query.Where("name = ?", opts.Name)
	}
	if opts.ID > 0 {
		query = query.Where("id = ?", opts.ID)
	}
	if err := query.Order(order(uint64(opts.AfterID), opts.Ascending)).Limit(queryLimit(opts.Limit)).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetHTTP returns one HTTP audit row scoped to clientID.
func (r *Recorder) GetHTTP(ctx context.Context, id auditid.ExchangeID, clientID string) (HTTPExchange, error) {
	if r == nil || !r.enabled {
		return HTTPExchange{}, gorm.ErrRecordNotFound
	}
	var row HTTPExchange
	query := r.db.WithContext(contextOrBackground(ctx)).Model(&HTTPExchange{}).Where("id = ?", id)
	if clientID != "" {
		query = query.Where("client_id = ?", clientID)
	}
	if err := query.First(&row).Error; err != nil {
		return HTTPExchange{}, err
	}
	return row, nil
}

// OpenStream opens the raw upgraded-stream spool file for row.
func (r *Recorder) OpenStream(row HTTPExchange) (*OpenedSpool, error) {
	if r == nil || !r.enabled || r.streamDir == "" {
		return nil, os.ErrNotExist
	}
	if row.StreamFile == "" {
		return nil, os.ErrNotExist
	}
	return openSpool(r.streamDir, row.StreamFile, "stream")
}

// OpenBody opens a request or response body spool file for row. A response
// body recorded by reference to the response cache (BodyFormatCache) is not
// here: the cache is the proxy's, and it resolves that reference itself.
func (r *Recorder) OpenBody(row HTTPExchange, kind string) (*OpenedSpool, error) {
	if r == nil || !r.enabled || r.bodyDir == "" {
		return nil, os.ErrNotExist
	}
	bodyFile := row.RequestBodyFile
	if kind == BodyKindResponse {
		bodyFile = row.ResponseBodyFile
	} else if kind != BodyKindRequest {
		return nil, os.ErrNotExist
	}
	if bodyFile == "" {
		return nil, os.ErrNotExist
	}
	opened, err := openSpool(r.bodyDir, bodyFile, "body")
	if err != nil || opened.TruncatedFrom > 0 {
		return opened, err
	}
	// The row recorded every byte the spool took, so a shorter file was cut
	// even if its marker was lost, as ADR 26-10-08-698 §6 has it.
	recorded := row.RequestBodyBytes
	if kind == BodyKindResponse {
		recorded = row.ResponseBodyBytes
	}
	if info, statErr := opened.Stat(); statErr == nil && info.Size() < recorded {
		opened.TruncatedFrom = recorded
	}
	return opened, nil
}

// Close flushes queued events.
func (r *Recorder) Close() error {
	if r == nil || !r.enabled {
		return nil
	}
	if r.closed.Swap(true) {
		return nil
	}
	r.enqueueMu.Lock()
	close(r.ch)
	close(r.dnsCh)
	r.enqueueMu.Unlock()
	r.wg.Wait()
	r.streamWg.Wait()
	if r.pools != nil {
		return r.pools.Close()
	}
	return nil
}

func (r *Recorder) enqueue(event any) {
	r.enqueueOn(r.ch, &r.dropped, event)
}

func (r *Recorder) enqueueOn(ch chan any, dropped *atomic.Uint64, event any) {
	if r == nil || !r.enabled {
		return
	}
	_, span := tracer().Start(eventContext(event), "proxy.audit.enqueue")
	defer span.End()
	r.enqueueMu.RLock()
	defer r.enqueueMu.RUnlock()
	if r.closed.Load() {
		dropped.Add(1)
		span.SetAttributes(attribute.Bool("proxy.audit.enqueued", false))
		span.SetStatus(codes.Error, "audit recorder closed")
		return
	}
	select {
	case ch <- event:
		span.SetAttributes(attribute.Bool("proxy.audit.enqueued", true))
	default:
		dropped.Add(1)
		span.SetAttributes(attribute.Bool("proxy.audit.enqueued", false))
		span.SetStatus(codes.Error, "audit queue full")
	}
}

func (r *Recorder) run() {
	defer r.wg.Done()
	defer close(r.done)
	ch, dns := r.ch, r.dnsCh
	for ch != nil || dns != nil {
		// HTTP and SOCKS first, whenever there are any: a DNS event is
		// written only when their queue is empty, so it never waits behind
		// more than one lookup. A closed queue reads as nil and drops out.
		select {
		case event, ok := <-ch:
			if !ok {
				ch = nil
				continue
			}
			r.write(event)
			continue
		default:
		}
		select {
		case event, ok := <-ch:
			if !ok {
				ch = nil
				continue
			}
			r.write(event)
		case event, ok := <-dns:
			if !ok {
				dns = nil
				continue
			}
			r.write(event)
		}
	}
}

func (r *Recorder) write(event any) {
	switch e := event.(type) {
	case HTTPEvent:
		_, span := tracer().Start(eventContext(e), "proxy.audit.write", trace.WithAttributes(attribute.String("proxy.audit.type", "http")))
		writtenAt := time.Now().UTC()
		err := r.db.Create(&HTTPExchange{
			CreatedAt:           nonZeroTime(e.Time),
			EnqueuedAt:          nonZeroTime(e.EnqueuedAt),
			WrittenAt:           writtenAt,
			ClientID:            e.ClientID,
			ClientSubject:       e.ClientSubject,
			ClientSerial:        e.ClientSerial,
			Method:              e.Method,
			URL:                 e.URL,
			Host:                e.Host,
			Status:              e.Status,
			DurationMillis:      e.Duration.Milliseconds(),
			DurationMicros:      e.Duration.Microseconds(),
			Blocked:             e.Blocked,
			BlockedReason:       e.BlockedReason,
			CacheHit:            e.CacheHit,
			CacheStored:         e.CacheStored,
			CacheKey:            e.CacheKey,
			CacheError:          e.CacheError,
			AppliedRuleID:       e.AppliedRuleID,
			AppliedPattern:      e.AppliedPattern,
			AppliedHeaders:      strings.Join(e.AppliedHeaders, ","),
			ForwardedURL:        e.ForwardedURL,
			SwappedSecretIDs:    strings.Join(e.SwappedSecretIDs, ","),
			RawBindingIDs:       strings.Join(e.RawBindingIDs, ","),
			RequestHeaders:      marshalHeaders(e.RequestHeaders, e.RedactRequestHeaders),
			ResponseHeaders:     marshalHeaders(e.ResponseHeaders, nil),
			ResponseBytes:       e.ResponseBytes,
			RequestBodyFile:     e.RequestBodyFile,
			RequestBodyFormat:   e.RequestBodyFormat,
			RequestBodyBytes:    e.RequestBodyBytes,
			RequestBodyError:    e.RequestBodyError,
			ResponseBodyFile:    e.ResponseBodyFile,
			ResponseBodyFormat:  e.ResponseBodyFormat,
			ResponseBodyBytes:   e.ResponseBodyBytes,
			ResponseBodyError:   e.ResponseBodyError,
			Upgrade:             e.Upgrade,
			UpgradeType:         e.UpgradeType,
			UpgradeC2SBytes:     e.UpgradeC2SBytes,
			UpgradeS2CBytes:     e.UpgradeS2CBytes,
			StreamSessionID:     e.StreamSessionID,
			StreamFile:          e.StreamFile,
			StreamFormat:        e.StreamFormat,
			StreamDroppedChunks: e.StreamDroppedChunks,
			StreamDroppedBytes:  e.StreamDroppedBytes,
			Tunnel:              e.Tunnel,
		}).Error
		recordError(span, err)
		span.End()
	case SOCKSEvent:
		_, span := tracer().Start(eventContext(e), "proxy.audit.write", trace.WithAttributes(attribute.String("proxy.audit.type", "socks")))
		writtenAt := time.Now().UTC()
		err := r.db.Create(&SOCKSConnect{
			CreatedAt:     nonZeroTime(e.Time),
			EnqueuedAt:    nonZeroTime(e.EnqueuedAt),
			WrittenAt:     writtenAt,
			ClientID:      e.ClientID,
			ClientSubject: e.ClientSubject,
			ClientSerial:  e.ClientSerial,
			Destination:   e.Destination,
			Port:          e.Port,
			Allowed:       e.Allowed,
			BlockedReason: e.BlockedReason,
		}).Error
		recordError(span, err)
		span.End()
	case DNSEvent:
		_, span := tracer().Start(eventContext(e), "proxy.audit.write", trace.WithAttributes(attribute.String("proxy.audit.type", "dns")))
		err := r.db.Create(&DNSQuery{
			CreatedAt:      nonZeroTime(e.Time),
			EnqueuedAt:     nonZeroTime(e.EnqueuedAt),
			WrittenAt:      time.Now().UTC(),
			ClientID:       e.ClientID,
			ClientSubject:  e.ClientSubject,
			ClientSerial:   e.ClientSerial,
			Name:           e.Name,
			Type:           e.Type,
			RCode:          e.RCode,
			Answers:        strings.Join(e.Answers, ","),
			DurationMillis: e.Duration.Milliseconds(),
			DurationMicros: e.Duration.Microseconds(),
			Error:          e.Error,
		}).Error
		recordError(span, err)
		span.End()
	}
}

func tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

func eventContext(event any) context.Context {
	switch e := event.(type) {
	case HTTPEvent:
		if e.Context != nil {
			return e.Context
		}
	case SOCKSEvent:
		if e.Context != nil {
			return e.Context
		}
	case DNSEvent:
		if e.Context != nil {
			return e.Context
		}
	}
	return context.Background()
}

func recordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func nonZeroTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

func marshalHeaders(headers http.Header, redactedHeaders []string) string {
	extraRedacted := make(map[string]struct{}, len(redactedHeaders))
	for _, header := range redactedHeaders {
		if header = strings.TrimSpace(header); header != "" {
			extraRedacted[normalizeHeaderKey(header)] = struct{}{}
		}
	}
	redacted := make(http.Header, len(headers))
	for key, values := range headers {
		if isSensitiveHeader(key) || isExtraRedactedHeader(key, extraRedacted) {
			redacted[key] = []string{"[REDACTED]"}
			continue
		}
		redacted[key] = append([]string(nil), values...)
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func applyHTTPQueryOptions(query *gorm.DB, opts QueryOptions) *gorm.DB {
	query = applyCursor(query, uint64(opts.AfterID), opts.Since, opts.Until)
	if opts.ClientID != "" {
		query = query.Where("client_id = ?", opts.ClientID)
	}
	if opts.Host != "" {
		if _, _, err := net.SplitHostPort(opts.Host); err == nil {
			query = query.Where("host = ?", opts.Host)
		} else {
			query = query.Where(`(host = ? OR host LIKE ? ESCAPE '\')`, opts.Host, escapeLike(opts.Host)+":%")
		}
	}
	if opts.MinStatus > 0 {
		query = query.Where("status >= ?", opts.MinStatus)
	}
	if opts.MaxStatus > 0 {
		query = query.Where("status <= ?", opts.MaxStatus)
	}
	if opts.Blocked != nil {
		query = query.Where("blocked = ?", *opts.Blocked)
	}
	return query.Limit(queryLimit(opts.Limit))
}

// escapeLike neutralizes the LIKE wildcards in a value matched literally: a
// host may hold "_", the single-character wildcard.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return replacer.Replace(value)
}

func applySOCKSQueryOptions(query *gorm.DB, opts QueryOptions) *gorm.DB {
	query = applyCursor(query, uint64(opts.AfterID), opts.Since, opts.Until)
	if opts.ClientID != "" {
		query = query.Where("client_id = ?", opts.ClientID)
	}
	if opts.Host != "" {
		query = query.Where("destination = ?", opts.Host)
	}
	return query.Limit(queryLimit(opts.Limit))
}

// applyCursor bounds a read by where the reader left off: after a row id, or
// within a time window. Rows are written in UTC (nonZeroTime), and SQLite
// compares times as text carrying their offset, so a time bound is compared in
// UTC too.
func applyCursor(query *gorm.DB, after uint64, since, until time.Time) *gorm.DB {
	if after > 0 {
		return query.Where("id > ?", after)
	}
	if !since.IsZero() {
		query = query.Where("created_at >= ?", since.UTC())
	}
	if !until.IsZero() {
		query = query.Where("created_at <= ?", until.UTC())
	}
	return query
}

func queryLimit(limit int) int {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	return limit
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func isSensitiveHeader(key string) bool {
	k := normalizeHeaderKey(key)
	if k == "authorization" || k == "cookie" || k == "set-cookie" || k == "proxy-authorization" {
		return true
	}
	return strings.HasSuffix(k, "-token") ||
		strings.HasSuffix(k, "-secret") ||
		strings.HasSuffix(k, "-api-key") ||
		strings.Contains(k, "credential")
}

func isExtraRedactedHeader(key string, extra map[string]struct{}) bool {
	if len(extra) == 0 {
		return false
	}
	_, ok := extra[normalizeHeaderKey(key)]
	return ok
}

func normalizeHeaderKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(key, "_", "-"))
}

func openRelativeSpoolFile(root, relativePath, label string) (*os.File, error) {
	cleaned := filepath.Clean(relativePath)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return nil, fmt.Errorf("invalid %s path", label)
	}
	fullPath := filepath.Join(root, cleaned)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fullAbs, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s path escapes spool dir", label)
	}
	return os.Open(fullAbs)
}

// retiredColumns are columns a database written by an earlier proxy may hold
// and this one never reads or writes, by table. AutoMigrate adds columns and
// never removes one, so each is dropped here, once, where it exists, with
// ALTER TABLE ... DROP COLUMN. That is not cheap: SQLite rewrites every row of
// the table to purge the column, and this is the largest table the pool keeps.
// It is a one-time rewrite, on the first start after the upgrade, before the
// proxy serves, and it takes time in proportion to the audit retained
// (Recording.Retention); every later start finds the column gone and does
// nothing.
//
// swapped_use_ids named the approved credential uses a request spent; uses
// went with the credential judge (ADR 26-10-27-880).
var retiredColumns = map[string][]string{
	"http_exchanges": {"swapped_use_ids"},
}

func dropRetiredColumns(db *gorm.DB) error {
	migrator := db.Migrator()
	for table, columns := range retiredColumns {
		for _, column := range columns {
			if !migrator.HasColumn(table, column) {
				continue
			}
			if err := db.Exec("ALTER TABLE ? DROP COLUMN ?", clause.Table{Name: table}, clause.Column{Name: column}).Error; err != nil {
				return fmt.Errorf("drop retired audit column %s.%s: %w", table, column, err)
			}
		}
	}
	return nil
}

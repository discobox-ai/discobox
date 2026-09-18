package audit

import (
	"context"
	"encoding/json"
	"fmt"
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
	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/auditid"
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
	SwappedUseIDs        []string
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
	// SwappedUseIDs names the approved credential uses this request spent, as
	// a comma-joined list (ADR 0130 §3). It is the join to the control plane's
	// credential_verdicts rows: one ID reaches both the verdict that authorized
	// a command and every request that actually spent the credential.
	//
	// Empty means no use was named, which is either an ordinary injected
	// sentinel or no swap at all. It never holds a sentinel — an ephemeral one
	// is a live bearer token, and this trail is kept to be read later.
	//
	// Deliberately unindexed. The only query that reads it matches one element
	// of the list, which is an expression with a leading wildcard and so cannot
	// use a B-tree index on the column; an index here would be write cost on
	// the highest-volume table in the schema and nothing else. The scan it
	// implies is bounded in practice by client_id, which is indexed and which
	// every real caller sends.
	SwappedUseIDs       string
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

// Recorder asynchronously persists audit events.
type Recorder struct {
	enabled         bool
	db              *gorm.DB
	ch              chan any
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
	// pools owns the database handle db borrows. Close releases it: without
	// that the connection outlives the recorder, which on Linux merely leaks
	// and on Windows makes the database file undeletable for the life of the
	// process.
	pools *gormdb.Pools
}

// QueryOptions filters audit reads. Limit defaults to 100 and is capped at 1000.
type QueryOptions struct {
	ClientID string
	Host     string
	// UseID narrows to the requests that spent one approved credential use.
	// It applies to HTTP reads only: a SOCKS connect tunnels bytes the proxy
	// never inspects, so no sentinel is ever swapped in one.
	UseID string
	// Since keeps rows written at or after it. Rows are written in UTC
	// (nonZeroTime), and SQLite compares times as text carrying their offset,
	// so the bound is compared in UTC too.
	Since time.Time
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
	// It takes precedence over Since and Ascending: with a cursor there is
	// nothing for a time bound to add, and the write order is forward by
	// definition.
	AfterID auditid.ExchangeID
	Limit   int
}

// order is the ORDER BY for opts. id breaks ties between rows written in the
// same instant, so a reader paging forward sees a stable sequence.
func (opts QueryOptions) order() string {
	if opts.AfterID > 0 {
		// Write order, which is what a cursor reads along. It is also the
		// primary key, so this is the one read here that needs no index.
		return "id ASC"
	}
	if opts.Ascending {
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
func (r *Recorder) BeginUpgradeStream(clientID, upgradeType string) (*StreamRecord, *StreamSession, error) {
	if r == nil || !r.enabled || r.streamDir == "" {
		return nil, nil, nil
	}
	record, session, err := BeginUpgradeStream(r.streamDir, clientID, upgradeType, r.streamQueueSize, &r.streamWg)
	if err != nil || record == nil {
		return record, session, err
	}
	session.onClose = r.trackSpool(record.File)
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
	spool.onClose = r.trackSpool(record.File)
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
	if err := pools.Write.WithContext(ctx).AutoMigrate(&HTTPExchange{}, &SOCKSConnect{}); err != nil {
		_ = pools.Close()
		return nil, err
	}
	r := &Recorder{
		enabled: true,
		db:      pools.Write,
		pools:   pools,
		ch:      make(chan any, queueSize),
		done:    make(chan struct{}),
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
	r.enqueue(event)
}

// RecordSOCKS queues a SOCKS audit event without blocking on SQLite.
func (r *Recorder) RecordSOCKS(event SOCKSEvent) {
	if event.EnqueuedAt.IsZero() {
		event.EnqueuedAt = time.Now().UTC()
	}
	r.enqueue(event)
}

// Dropped returns the number of events dropped due to backpressure.
func (r *Recorder) Dropped() uint64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// ListHTTP returns recent HTTP audit exchanges newest first.
func (r *Recorder) ListHTTP(ctx context.Context, opts QueryOptions) ([]HTTPExchange, error) {
	if r == nil || !r.enabled {
		return nil, nil
	}
	var rows []HTTPExchange
	query := applyHTTPQueryOptions(r.db.WithContext(contextOrBackground(ctx)).Model(&HTTPExchange{}), opts)
	if err := query.Order(opts.order()).Find(&rows).Error; err != nil {
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
	if err := query.Order(opts.order()).Find(&rows).Error; err != nil {
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
func (r *Recorder) OpenStream(row HTTPExchange) (*os.File, error) {
	if r == nil || !r.enabled || r.streamDir == "" {
		return nil, os.ErrNotExist
	}
	if row.StreamFile == "" {
		return nil, os.ErrNotExist
	}
	cleaned := filepath.Clean(row.StreamFile)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return nil, fmt.Errorf("invalid stream path")
	}
	fullPath := filepath.Join(r.streamDir, cleaned)
	streamRoot, err := filepath.Abs(r.streamDir)
	if err != nil {
		return nil, err
	}
	fullAbs, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, err
	}
	if fullAbs != streamRoot && !strings.HasPrefix(fullAbs, streamRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("stream path escapes stream dir")
	}
	return os.Open(fullAbs)
}

// OpenBody opens a request or response body spool file for row.
func (r *Recorder) OpenBody(row HTTPExchange, kind string) (*os.File, error) {
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
	return openRelativeSpoolFile(r.bodyDir, bodyFile, "body")
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
	r.enqueueMu.Unlock()
	r.wg.Wait()
	r.streamWg.Wait()
	if r.pools != nil {
		return r.pools.Close()
	}
	return nil
}

func (r *Recorder) enqueue(event any) {
	if r == nil || !r.enabled {
		return
	}
	_, span := tracer().Start(eventContext(event), "proxy.audit.enqueue")
	defer span.End()
	r.enqueueMu.RLock()
	defer r.enqueueMu.RUnlock()
	if r.closed.Load() {
		r.dropped.Add(1)
		span.SetAttributes(attribute.Bool("proxy.audit.enqueued", false))
		span.SetStatus(codes.Error, "audit recorder closed")
		return
	}
	select {
	case r.ch <- event:
		span.SetAttributes(attribute.Bool("proxy.audit.enqueued", true))
	default:
		r.dropped.Add(1)
		span.SetAttributes(attribute.Bool("proxy.audit.enqueued", false))
		span.SetStatus(codes.Error, "audit queue full")
	}
}

func (r *Recorder) run() {
	defer r.wg.Done()
	defer close(r.done)
	for event := range r.ch {
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
				SwappedUseIDs:       strings.Join(e.SwappedUseIDs, ","),
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
		}
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
	query = applyCursor(query, opts)
	if opts.ClientID != "" {
		query = query.Where("client_id = ?", opts.ClientID)
	}
	if opts.Host != "" {
		query = query.Where("host = ?", opts.Host)
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
	if opts.UseID != "" {
		// Match a whole element of the comma-joined list rather than a
		// substring: padding both sides with the delimiter is what keeps one
		// use ID from matching another that merely contains it.
		// ESCAPE is named explicitly: SQLite has no default escape character,
		// so without it the escaping below would match a literal backslash.
		query = query.Where(`',' || swapped_use_ids || ',' LIKE ? ESCAPE '\'`, "%,"+escapeLike(opts.UseID)+",%")
	}
	return query.Limit(queryLimit(opts.Limit))
}

// escapeLike neutralizes the LIKE wildcards in a value matched literally.
//
// This is load-bearing rather than defensive: a use ID is id.New("use"), which
// is "use_" plus random text, so every single one of them contains the
// single-character wildcard. Unescaped, use_abc would also match usexabc.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return replacer.Replace(value)
}

func applySOCKSQueryOptions(query *gorm.DB, opts QueryOptions) *gorm.DB {
	query = applyCursor(query, opts)
	if opts.ClientID != "" {
		query = query.Where("client_id = ?", opts.ClientID)
	}
	if opts.Host != "" {
		query = query.Where("destination = ?", opts.Host)
	}
	return query.Limit(queryLimit(opts.Limit))
}

// applyCursor bounds a read by where the reader left off: after a row id, or
// at a time. Rows are written in UTC (nonZeroTime), and SQLite compares times
// as text carrying their offset, so a time bound is compared in UTC too.
func applyCursor(query *gorm.DB, opts QueryOptions) *gorm.DB {
	switch {
	case opts.AfterID > 0:
		return query.Where("id > ?", opts.AfterID)
	case !opts.Since.IsZero():
		return query.Where("created_at >= ?", opts.Since.UTC())
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

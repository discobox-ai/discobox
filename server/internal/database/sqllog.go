package database

import (
	"context"
	"io"
	"log"
	"time"

	"gorm.io/gorm/logger"
)

// slowSQL is how long a statement runs before it is logged.
const slowSQL = 200 * time.Millisecond

// A statement's bound values are whatever its row holds, a private key or a
// secret among them, and with no encryption provider a sealed value is only
// base64 of the plaintext. So no log line of this process carries one.
//
// GORM logs Scan through logger.Recorder, which renders the statement by the
// process-wide RecorderParamsFilter rather than by the logger's
// ParameterizedQueries, and hands the logger the finished text. Its default
// keeps the values; this one drops them. It is set once, here, before any
// handle exists: assigning it later would race the statements already running.
func init() {
	logger.RecorderParamsFilter = func(_ context.Context, sql string, _ ...any) (string, []any) {
		return sql, nil
	}
}

// newSQLLogger is the logger of every handle New opens. It logs a slow or
// failed statement with its placeholders and never its bound values.
func newSQLLogger(w io.Writer, slow time.Duration) logger.Interface {
	return logger.New(log.New(w, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold:             slow,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		Colorful:                  true,
		ParameterizedQueries:      true,
	})
}

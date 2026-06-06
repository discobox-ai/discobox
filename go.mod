module github.com/obot-platform/disco2

go 1.26

require (
	github.com/google/uuid v1.6.0
	gorm.io/gorm v1.31.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/glebarez/sqlite v1.11.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.6.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.21 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	gorm.io/driver/postgres v1.6.0 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)

require (
	github.com/danielgtaylor/huma/v2 v2.38.0
	github.com/go-chi/chi/v5 v5.3.0
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/obot-platform/disco2/gormdb v0.0.0
	github.com/obot-platform/disco2/jobqueue v0.0.0
	golang.org/x/text v0.36.0 // indirect
)

replace github.com/obot-platform/disco2/gormdb => ./gormdb

replace github.com/obot-platform/disco2/jobqueue => ./jobqueue

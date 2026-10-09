module github.com/discobox-ai/discobox/pool-agent

go 1.27.1

replace github.com/discobox-ai/discobox => ..

require (
	aidanwoods.dev/go-paseto v1.6.0
	github.com/containerd/errdefs v1.0.0
	github.com/discobox-ai/discobox v0.0.0-00010101000000-000000000000
	github.com/discobox-ai/x v0.0.0-20260928053835-39c36487e906
	github.com/dustin/go-humanize v1.0.1
	github.com/fsnotify/fsnotify v1.10.1
	github.com/go-chi/chi/v5 v5.3.0
	github.com/go-faster/errors v0.7.1
	github.com/go-faster/jx v1.2.0
	github.com/hashicorp/yamux v0.1.2
	github.com/moby/buildkit v0.20.2
	github.com/moby/moby/api v1.56.0
	github.com/moby/moby/client v0.6.0
	github.com/ogen-go/ogen v1.20.3
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/metric v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	aidanwoods.dev/go-result v0.3.1 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/containerd/typeurl/v2 v2.2.3 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/docker/go-connections v0.8.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/ebitengine/purego v0.10.2 // indirect
	github.com/elazarl/goproxy v1.9.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/glebarez/sqlite v1.11.0 // indirect
	github.com/go-faster/yaml v0.4.6 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/pprof v0.0.0-20260802141513-ef3492d7dac3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/mdlayher/vsock v1.3.0 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/things-go/go-socks5 v0.1.1 // indirect
	github.com/tonistiigi/fsutil v0.0.0-20250113203817-b14e27f4135a // indirect
	github.com/tursodatabase/turso-go-platform-libs v0.7.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260921155816-b14227669459 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gorm.io/driver/postgres v1.6.2 // indirect
	gorm.io/gorm v1.31.2 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
	turso.tech/database/tursogo v0.7.2 // indirect
)

tool github.com/ogen-go/ogen/cmd/ogen

// goproxy drops the bytes a client sends in the same write as an upgrade
// request, hanging both ends; see the root go.mod for the detail. Repeated here
// because a replace only applies to the main module, and this module builds
// proxy itself.
//
// Drop this with the one in the root go.mod, once an upstream release fixes both
// call sites (elazarl/goproxy#805 covers only the MITM half).
replace github.com/elazarl/goproxy => github.com/discobox-ai/goproxy v0.0.0-20260912041536-a9c9419932d8

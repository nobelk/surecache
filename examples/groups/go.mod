module github.com/nobelk/surecache/examples/groups

go 1.25

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/nobelk/surecache v0.0.0
	github.com/nobelk/surecache/promobs v0.0.0
	github.com/nobelk/surecache/surecachepg v0.0.0
	github.com/prometheus/client_golang v1.24.1
	golang.org/x/sync v0.22.0
)

// Security floor: GO-2026-6629 is fixed in v0.41.0.
require golang.org/x/text v0.41.0 // indirect

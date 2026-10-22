module github.com/nobelk/surecache/surecachepg

go 1.25

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/nobelk/surecache v0.0.0
)

// Security floor: GO-2026-6629 is fixed in v0.41.0.
require golang.org/x/text v0.41.0 // indirect

module github.com/dsh-fleet/dsh-fleet

go 1.23

require github.com/jackc/pgx/v5 v5.7.1

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.27.0 // indirect
	golang.org/x/sync v0.8.0 // indirect
	golang.org/x/text v0.18.0 // indirect
)

// The uplink router adds github.com/coder/websocket as a direct dependency
// when it lands. golang.org/x/crypto is already here indirectly via pgx and
// becomes direct at the same time, for argon2id password and token hashing.

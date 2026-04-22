package persistence

import "context"

// BatchMode controls Cassandra batch semantics without exposing gocql upstream.
type BatchMode int

const (
	LoggedBatch BatchMode = iota
	UnloggedBatch
)

// Statement is one persistence operation.
type Statement struct {
	CQL        string
	Args       []any
	Idempotent bool
}

// Scanner is the minimal row scanner shape needed by application stores.
type Scanner interface {
	Scan(dest ...any) bool
}

// Store is a thin persistence primitive.
// It intentionally does not know business schemas, partition keys, versions, or idempotency rules.
type Store interface {
	ExecuteBatch(ctx context.Context, mode BatchMode, statements []Statement) error
	Query(ctx context.Context, cql string, args []any, scan func(Scanner) error) error
	Close()
}

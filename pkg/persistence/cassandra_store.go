package persistence

import (
	"context"
	"fmt"
	"time"

	"github.com/gocql/gocql"
	"sync/atomic"
)

// ErrEmptyBatch indicates a caller attempted to execute an empty persistence batch.
var ErrEmptyBatch = fmt.Errorf("persistence: statements slice must not be empty")

// CassandraStore is a thin Cassandra adapter.
// It owns the session and executes caller-provided CQL; business schemas live above this package.
type CassandraStore struct {
	session *gocql.Session
}

// processPersistenceWriteDurationNsSum / processPersistenceWriteDurationNsCount
// track cumulative Cassandra batch-write time inside the current node process.
// They are process-local aggregates, not per-actor and not cluster-global.
var (
	processPersistenceWriteDurationNsSum   atomic.Uint64
	processPersistenceWriteDurationNsCount atomic.Uint64
)

// NewCassandraStore creates and verifies a Cassandra session.
func NewCassandraStore(hosts []string, keyspace string) (*CassandraStore, error) {
	cluster := gocql.NewCluster(hosts...)
	cluster.Keyspace = keyspace
	cluster.Consistency = gocql.Quorum
	cluster.Timeout = 5 * time.Second
	cluster.ConnectTimeout = 10 * time.Second

	session, err := cluster.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("persistence: failed to create cassandra session: %w", err)
	}
	return &CassandraStore{session: session}, nil
}

func (s *CassandraStore) Close() {
	s.session.Close()
}

func (s *CassandraStore) ExecuteBatch(ctx context.Context, mode BatchMode, statements []Statement) error {
	if len(statements) == 0 {
		return ErrEmptyBatch
	}

	batch := s.session.NewBatch(toGocqlBatchType(mode)).WithContext(ctx)
	for i := range statements {
		stmt := &statements[i]
		batch.Entries = append(batch.Entries, gocql.BatchEntry{
			Stmt:       stmt.CQL,
			Args:       stmt.Args,
			Idempotent: stmt.Idempotent,
		})
	}

	writeStart := time.Now()
	err := s.session.ExecuteBatch(batch)
	processPersistenceWriteDurationNsSum.Add(uint64(time.Since(writeStart)))
	processPersistenceWriteDurationNsCount.Add(1)
	if err != nil {
		return fmt.Errorf("persistence: ExecuteBatch failed: %w", err)
	}
	return nil
}

func (s *CassandraStore) Query(ctx context.Context, cql string, args []any, scan func(Scanner) error) error {
	iter := s.session.Query(cql, args...).WithContext(ctx).Iter()
	if scanErr := scan(iter); scanErr != nil {
		_ = iter.Close()
		return scanErr
	}
	if err := iter.Close(); err != nil {
		return fmt.Errorf("persistence: Query failed: %w", err)
	}
	return nil
}

func toGocqlBatchType(mode BatchMode) gocql.BatchType {
	if mode == LoggedBatch {
		return gocql.LoggedBatch
	}
	return gocql.UnloggedBatch
}

// ProcessPersistenceWriteDurationNsSum returns cumulative batch-write time
// (nanoseconds) since this process started.
func ProcessPersistenceWriteDurationNsSum() uint64 {
	return processPersistenceWriteDurationNsSum.Load()
}

// ProcessPersistenceWriteDurationNsCount returns cumulative ExecuteBatch call count
// since this process started.
func ProcessPersistenceWriteDurationNsCount() uint64 {
	return processPersistenceWriteDurationNsCount.Load()
}

package persistence

import (
	"context"
	"errors"
	"testing"
)

type recordingStore struct {
	batches [][]Statement
}

func (s *recordingStore) ExecuteBatch(_ context.Context, _ BatchMode, statements []Statement) error {
	if len(statements) == 0 {
		return ErrEmptyBatch
	}
	cp := append([]Statement(nil), statements...)
	s.batches = append(s.batches, cp)
	return nil
}

func (s *recordingStore) Query(context.Context, string, []any, func(Scanner) error) error {
	return nil
}

func (s *recordingStore) Close() {}

func TestStoreInterface_ExecuteBatchIsSchemaAgnostic(t *testing.T) {
	t.Parallel()

	store := &recordingStore{}
	err := store.ExecuteBatch(context.Background(), UnloggedBatch, []Statement{
		{CQL: "INSERT INTO any_table (id, payload) VALUES (?, ?)", Args: []any{1, []byte("x")}, Idempotent: true},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch returned error: %v", err)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 1 {
		t.Fatalf("expected one recorded statement, got %+v", store.batches)
	}
	if store.batches[0][0].CQL == "" {
		t.Fatal("expected caller-provided CQL to be preserved")
	}
}

func TestStoreInterface_EmptyBatchError(t *testing.T) {
	t.Parallel()

	store := &recordingStore{}
	err := store.ExecuteBatch(context.Background(), UnloggedBatch, nil)
	if !errors.Is(err, ErrEmptyBatch) {
		t.Fatalf("expected ErrEmptyBatch, got %v", err)
	}
}

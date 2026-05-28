package main

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

type stubEmbedder struct {
	vec []float32
	err error
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return s.vec, s.err
}
func (s *stubEmbedder) Model() string { return "stub-model" }

func TestMemoryEmbedLoopRunOnceWritesBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loop := newMemoryEmbedLoop(db, &stubEmbedder{vec: []float32{0.1, 0.2}})
	loop.batch = 2

	rows := sqlmock.NewRows([]string{"id", "content"}).
		AddRow("m1", "hello world").
		AddRow("m2", "another")
	mock.ExpectQuery(regexp.QuoteMeta("FROM memories")).
		WithArgs(2).
		WillReturnRows(rows)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE memories")).
		WithArgs("[0.100000,0.200000]", "stub-model", "m1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE memories")).
		WithArgs("[0.100000,0.200000]", "stub-model", "m2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	loop.runOnce()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMemoryEmbedLoopSkipsOnEmbedderFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loop := newMemoryEmbedLoop(db, &stubEmbedder{err: errors.New("rate limit")})
	loop.batch = 1

	mock.ExpectQuery(regexp.QuoteMeta("FROM memories")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content"}).AddRow("m1", "abc"))
	// No UPDATE expected since embed errored.

	loop.runOnce()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMemoryEmbedLoopShortCircuitsWithoutEmbedder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loop := newMemoryEmbedLoop(db, nil)
	loop.runOnce()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIsPgvectorMissing(t *testing.T) {
	cases := map[string]bool{
		"":                                    false,
		"connection refused":                  false,
		`pq: type "vector" does not exist`:    true,
		`column "embedding" does not exist`:   true,
		`pq: extension "vector" is not avail`: true,
	}
	for msg, want := range cases {
		var err error
		if msg != "" {
			err = errors.New(msg)
		}
		got := isPgvectorMissing(err)
		if got != want {
			t.Fatalf("isPgvectorMissing(%q): want %v got %v", msg, want, got)
		}
	}
}

func TestTruncForEmbed(t *testing.T) {
	if got := truncForEmbed("   "); got != "" {
		t.Fatalf("blank: got %q", got)
	}
	long := make([]byte, memoryEmbedMaxInputChars+50)
	for i := range long {
		long[i] = 'a'
	}
	out := truncForEmbed(string(long))
	if len(out) != memoryEmbedMaxInputChars {
		t.Fatalf("expected truncation to %d, got %d", memoryEmbedMaxInputChars, len(out))
	}
}

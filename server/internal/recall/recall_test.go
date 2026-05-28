package recall

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestNewDefaultsAreSane(t *testing.T) {
	s := New(nil)
	if s.EmbeddingDimension != 1536 {
		t.Fatalf("default dim should be 1536, got %d", s.EmbeddingDimension)
	}
	if s.SimilarityFloor <= 0 || s.SimilarityFloor > 1 {
		t.Fatalf("similarity floor out of range: %v", s.SimilarityFloor)
	}
}

func TestQueryReturnsErrOnEmptyEmbedding(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := New(db)
	_, err := s.Query(context.Background(), "f", "", nil, 5)
	if !errors.Is(err, ErrNoEmbedding) {
		t.Fatalf("expected ErrNoEmbedding, got %v", err)
	}
}

func TestQueryRejectsDimMismatch(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	s := New(db)
	s.EmbeddingDimension = 4
	_, err := s.Query(context.Background(), "f", "", []float32{1, 2}, 5)
	if err == nil || !strings.Contains(err.Error(), "dim mismatch") {
		t.Fatalf("expected dim mismatch error, got %v", err)
	}
}

func TestQueryShortCircuitsOnNilDB(t *testing.T) {
	s := New(nil)
	out, err := s.Query(context.Background(), "f", "", []float32{1}, 5)
	if err != nil {
		t.Fatalf("nil db should not error, got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil out, got %v", out)
	}
}

func TestQueryFiltersByFundAndLayer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dim := 4
	embed := []float32{0.1, 0.2, 0.3, 0.4}
	s := New(db)
	s.EmbeddingDimension = dim
	// 0 floor to keep query simpler
	s.SimilarityFloor = 0

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "fund_id", "layer", "title", "content", "tags", "created_at", "similarity"}).
		AddRow("m1", "fund-a", "agent", "Title One", strings.Repeat("x", 300), []byte("{a,b}"), now, 0.91).
		AddRow("m2", "fund-a", "agent", "Title Two", "short content", []byte("{}"), now, 0.72)

	mock.ExpectQuery(regexp.QuoteMeta("FROM memories")).
		WithArgs("[0.100000,0.200000,0.300000,0.400000]", 3, "fund-a", "agent").
		WillReturnRows(rows)

	results, err := s.Query(context.Background(), "fund-a", "agent", embed, 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].MemoryID != "m1" || results[0].Similarity != 0.91 {
		t.Fatalf("row 0 unexpected: %+v", results[0])
	}
	if !strings.HasSuffix(results[0].Snippet, "…") {
		t.Fatalf("long content should be truncated with ellipsis, got %q", results[0].Snippet)
	}
	if len(results[0].Tags) != 2 || results[0].Tags[0] != "a" {
		t.Fatalf("tags not parsed: %+v", results[0].Tags)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestVectorLiteralFormatsList(t *testing.T) {
	got := vectorLiteral([]float32{1, 2.5, -0.5})
	want := "[1.000000,2.500000,-0.500000]"
	if got != want {
		t.Fatalf("vectorLiteral: want %q got %q", want, got)
	}
}

func TestSnippetUTF8Safe(t *testing.T) {
	s := strings.Repeat("中", 300)
	out := snippet(s, 20)
	// 20 runes plus ellipsis
	runeCount := 0
	for range out {
		runeCount++
	}
	if runeCount != 21 {
		t.Fatalf("snippet rune count: want 21, got %d (%s)", runeCount, out)
	}
}

func TestTagsScannerHandlesNull(t *testing.T) {
	var tags []string
	ts := tagsScannerFor(&tags)
	if err := ts.Scan(nil); err != nil {
		t.Fatalf("nil scan: %v", err)
	}
	if tags != nil {
		t.Fatalf("expected nil tags, got %v", tags)
	}
}

func TestTagsScannerParsesPQ(t *testing.T) {
	var tags []string
	ts := tagsScannerFor(&tags)
	if err := ts.Scan([]byte(`{alpha,beta,"with space"}`)); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(tags) != 3 || tags[2] != "with space" {
		t.Fatalf("parsed: %v", tags)
	}
}

package fx

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockedRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func TestIsSupported(t *testing.T) {
	cases := map[string]bool{
		"USD": true, "usd": true, " USD ": true,
		"CNY": true, "HKD": true, "EUR": true,
		"":    false,
		"BTC": false,
	}
	for in, want := range cases {
		if got := IsSupported(in); got != want {
			t.Errorf("IsSupported(%q)=%v want %v", in, got, want)
		}
	}
}

func TestSameCurrency(t *testing.T) {
	if !SameCurrency("USD", "usd") {
		t.Error("USD/usd should be same")
	}
	if SameCurrency("USD", "CNY") {
		t.Error("USD/CNY should differ")
	}
}

func TestComputeTriangulated(t *testing.T) {
	usdToCNY := &Rate{Rate: 7.20}
	usdToHKD := &Rate{Rate: 7.80}
	r, ok := computeTriangulated(usdToCNY, usdToHKD)
	if !ok {
		t.Fatal("expected ok")
	}
	want := 7.80 / 7.20 // CNY → HKD
	if roundFloat(r, 6) != roundFloat(want, 6) {
		t.Errorf("got %v want %v", r, want)
	}
}

func TestComputeTriangulated_BadInputs(t *testing.T) {
	if _, ok := computeTriangulated(nil, &Rate{Rate: 1}); ok {
		t.Error("nil should fail")
	}
	if _, ok := computeTriangulated(&Rate{Rate: 0}, &Rate{Rate: 1}); ok {
		t.Error("zero base should fail")
	}
}

func TestRepo_Upsert_RejectsInvalid(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	cases := []struct {
		name string
		p    UpsertParams
	}{
		{"empty base", UpsertParams{Quote: "USD", Rate: 1.0}},
		{"empty quote", UpsertParams{Base: "USD", Rate: 1.0}},
		{"same currency", UpsertParams{Base: "USD", Quote: "USD", Rate: 1.0}},
		{"unsupported", UpsertParams{Base: "USD", Quote: "BTC", Rate: 1.0}},
		{"zero rate", UpsertParams{Base: "USD", Quote: "CNY", Rate: 0}},
		{"negative rate", UpsertParams{Base: "USD", Quote: "CNY", Rate: -1}},
		{"bad source", UpsertParams{Base: "USD", Quote: "CNY", Rate: 1.0, Source: "twitter"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := repo.Upsert(context.Background(), c.p); err == nil {
				t.Errorf("expected error for %+v", c.p)
			}
		})
	}
}

func TestRepo_Upsert_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO fx_rates")).
		WithArgs("USD", "CNY", 7.20, sqlmock.AnyArg(), "yahoo", "", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rate-1"))
	id, err := repo.Upsert(context.Background(), UpsertParams{
		Base: "usd", Quote: "cny", Rate: 7.20, Source: "yahoo",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id != "rate-1" {
		t.Errorf("id = %q", id)
	}
}

func TestRepo_AsOf_IdentityShortCircuit(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	r, err := repo.AsOf(context.Background(), "USD", "USD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Rate != 1.0 || r.Source != "identity" {
		t.Errorf("unexpected rate = %+v", r)
	}
}

func TestRepo_AsOf_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "CNY", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.AsOf(context.Background(), "USD", "CNY", time.Now()); !errors.Is(err, ErrRateNotFound) {
		t.Errorf("err = %v, want ErrRateNotFound", err)
	}
}

func TestRepo_AsOf_Found(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "CNY", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("USD", "CNY", 7.18, now, "yahoo"))
	r, err := repo.AsOf(context.Background(), "USD", "CNY", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Rate != 7.18 || r.Source != "yahoo" {
		t.Errorf("rate = %+v", r)
	}
}

func TestRepo_Convert_Identity(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	out, _, err := repo.Convert(context.Background(), 100.0, "USD", "USD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != 100.0 {
		t.Errorf("out = %v", out)
	}
}

func TestRepo_Convert_DirectRate(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "CNY", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("USD", "CNY", 7.18, now, "yahoo"))
	out, r, err := repo.Convert(context.Background(), 100.0, "USD", "CNY", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != 100.0*7.18 {
		t.Errorf("out = %v", out)
	}
	if r.Rate != 7.18 {
		t.Errorf("rate = %+v", r)
	}
}

func TestRepo_Convert_Triangulate(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	// 1) direct CNY/HKD lookup misses
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("CNY", "HKD", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	// 2) USD/CNY = 7.20
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "CNY", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("USD", "CNY", 7.20, now, "yahoo"))
	// 3) USD/HKD = 7.80
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "HKD", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("USD", "HKD", 7.80, now, "yahoo"))

	out, r, err := repo.Convert(context.Background(), 100.0, "CNY", "HKD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := 100.0 * (7.80 / 7.20)
	if roundFloat(out, 6) != roundFloat(want, 6) {
		t.Errorf("out = %v, want %v", out, want)
	}
	if r.Source != "triangulated" {
		t.Errorf("source = %q", r.Source)
	}
}

func TestRepo_Convert_TriangulateInverse(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	// 1) direct CNY/HKD lookup misses
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("CNY", "HKD", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	// 2) USD/CNY misses
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "CNY", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	// 3) USD/HKD misses (forces inverse direction)
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("USD", "HKD", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	// 4) CNY/USD = 1/7.20
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("CNY", "USD", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("CNY", "USD", 1.0/7.20, now, "yahoo"))
	// 5) HKD/USD = 1/7.80
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("HKD", "USD", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("HKD", "USD", 1.0/7.80, now, "yahoo"))

	out, r, err := repo.Convert(context.Background(), 100.0, "CNY", "HKD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Source != "triangulated_inverse" {
		t.Errorf("source = %q", r.Source)
	}
	want := 100.0 * (7.80 / 7.20)
	if roundFloat(out, 4) != roundFloat(want, 4) {
		t.Errorf("out = %v, want %v", out, want)
	}
}

func TestRepo_Convert_Missing(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	for i := 0; i < 5; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
			WillReturnError(sql.ErrNoRows)
	}
	if _, _, err := repo.Convert(context.Background(), 100, "CNY", "HKD", time.Now()); !errors.Is(err, ErrRateNotFound) {
		t.Errorf("err = %v, want ErrRateNotFound", err)
	}
}

func TestRepo_ListRecent(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("USD", "CNY", 7.18, now, "yahoo"))
	rows, err := repo.ListRecent(context.Background(), ListRecentParams{Limit: 50})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %+v", rows)
	}
}

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockedBrokerLinkRepo(t *testing.T) (*BrokerLinkRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewBrokerLinkRepo(db), mock, func() { _ = db.Close() }
}

// Column list mirrors the SELECT in scanBrokerLink so the rowset
// the mock returns lines up with the Scan slot count.
var brokerLinkColumns = []string{
	"id", "fund_id", "user_id", "broker_id", "account_id", "status",
	"approved_by", "approved_at", "credentials_encrypted", "metadata",
	"created_at", "updated_at",
}

func TestBrokerLinkRepo_GetActiveByFundID_Found(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			"link-1", "fund-1", "user-1", "ibkr", "U1234567", "active",
			"approver-1", now, []byte("ct"), []byte(`{"region":"us"}`),
			now, now,
		))
	link, err := repo.GetActiveByFundID(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !link.IsActive() {
		t.Errorf("IsActive = false, want true (status=%s)", link.Status)
	}
	if link.BrokerID != "ibkr" {
		t.Errorf("BrokerID = %q, want ibkr", link.BrokerID)
	}
	if string(link.Metadata) != `{"region":"us"}` {
		t.Errorf("Metadata = %s", string(link.Metadata))
	}
}

func TestBrokerLinkRepo_GetActiveByFundID_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs("fund-1").
		WillReturnError(sql.ErrNoRows)
	_, err := repo.GetActiveByFundID(context.Background(), "fund-1")
	if !errors.Is(err, ErrBrokerLinkNotFound) {
		t.Errorf("err = %v, want ErrBrokerLinkNotFound", err)
	}
}

func TestBrokerLinkRepo_GetActiveByFundID_EmptyID(t *testing.T) {
	repo, _, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	if _, err := repo.GetActiveByFundID(context.Background(), ""); !errors.Is(err, ErrBrokerLinkNotFound) {
		t.Errorf("err = %v, want ErrBrokerLinkNotFound (empty id should not hit DB)", err)
	}
}

func TestBrokerLinkRepo_GetActiveByFundID_DefaultsEmptyMetadata(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			"link-1", "fund-1", "user-1", "ibkr", "U1234567", "active",
			nil, nil, nil, []byte{}, // metadata stored as zero-length blob
			now, now,
		))
	link, err := repo.GetActiveByFundID(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(link.Metadata) != "{}" {
		t.Errorf("Metadata = %s, want {}", string(link.Metadata))
	}
}

func TestBrokerLinkRepo_Create_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO broker_links`)).
		WithArgs("fund-1", "user-1", "ibkr", "U1234567",
			[]byte(nil), []byte(`{}`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("link-1"))
	id, err := repo.Create(context.Background(), CreateParams{
		FundID:    "fund-1",
		UserID:    "user-1",
		BrokerID:  "ibkr",
		AccountID: "U1234567",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id != "link-1" {
		t.Errorf("id = %q, want link-1", id)
	}
}

func TestBrokerLinkRepo_Create_PassesMetadataAndCreds(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	creds := []byte{0x01, 0x02, 0x03}
	meta := json.RawMessage(`{"sub_account":"main"}`)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO broker_links`)).
		WithArgs("fund-1", "user-1", "ibkr", "U1234567", creds, []byte(meta)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("link-1"))
	if _, err := repo.Create(context.Background(), CreateParams{
		FundID:               "fund-1",
		UserID:               "user-1",
		BrokerID:             "ibkr",
		AccountID:            "U1234567",
		CredentialsEncrypted: creds,
		Metadata:             meta,
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerLinkRepo_Create_RejectsMissingFields(t *testing.T) {
	repo, _, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	cases := []CreateParams{
		{},
		{FundID: "f"},
		{FundID: "f", UserID: "u"},
		{FundID: "f", UserID: "u", BrokerID: "ibkr"},
	}
	for i, p := range cases {
		if _, err := repo.Create(context.Background(), p); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestBrokerLinkRepo_Approve_MovesPendingToActive(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs("link-1", "approver-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Approve(context.Background(), "link-1", "approver-1"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerLinkRepo_Approve_NotFoundWhenAlreadyActive(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs("link-1", "approver-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Approve(context.Background(), "link-1", "approver-1"); !errors.Is(err, ErrBrokerLinkNotFound) {
		t.Errorf("err = %v, want ErrBrokerLinkNotFound (already active or revoked)", err)
	}
}

func TestBrokerLinkRepo_Revoke_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs("link-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Revoke(context.Background(), "link-1"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerLinkRepo_Revoke_NotFoundWhenAlreadyRevoked(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs("link-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Revoke(context.Background(), "link-1"); !errors.Is(err, ErrBrokerLinkNotFound) {
		t.Errorf("err = %v, want ErrBrokerLinkNotFound", err)
	}
}

func TestBrokerLinkRepo_ListByFundID_OrdersDesc(t *testing.T) {
	repo, mock, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	t1 := time.Now()
	t2 := t1.Add(-time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).
			AddRow("link-2", "fund-1", "user-1", "ibkr", "U222", "pending",
				nil, nil, nil, []byte(`{}`), t1, t1).
			AddRow("link-1", "fund-1", "user-1", "ibkr", "U111", "revoked",
				nil, nil, nil, []byte(`{}`), t2, t2))
	links, err := repo.ListByFundID(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("len = %d, want 2", len(links))
	}
	if links[0].ID != "link-2" {
		t.Errorf("first link = %s, want link-2 (newest first)", links[0].ID)
	}
}

func TestBrokerLinkRepo_ListByFundID_EmptyID(t *testing.T) {
	repo, _, cleanup := newMockedBrokerLinkRepo(t)
	defer cleanup()
	links, err := repo.ListByFundID(context.Background(), "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if links != nil {
		t.Errorf("links = %v, want nil for empty fund id", links)
	}
}

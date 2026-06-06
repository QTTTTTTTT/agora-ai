package main

import "testing"

func TestTemplatizeAPIPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Pass-through.
		{name: "non-api", in: "/health", want: "/health"},
		{name: "api root", in: "/api", want: "/api"},
		{name: "api root slash", in: "/api/", want: "/api/"},
		{name: "static api path", in: "/api/health", want: "/api/health"},
		{name: "static deep", in: "/api/companies/overview", want: "/api/companies/overview"},
		{name: "auth", in: "/api/auth/login", want: "/api/auth/login"},
		{name: "enum segment", in: "/api/admin/funds/live", want: "/api/admin/funds/live"},

		// UUID.
		{name: "uuid",
			in:   "/api/funds/550e8400-e29b-41d4-a716-446655440000/holdings",
			want: "/api/funds/{id}/holdings"},
		{name: "two uuids",
			in:   "/api/funds/550e8400-e29b-41d4-a716-446655440000/plans/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			want: "/api/funds/{id}/plans/{id}"},

		// All-digit IDs.
		{name: "numeric id",
			in:   "/api/orders/123456",
			want: "/api/orders/{id}"},
		{name: "numeric id short", // 5 digits stays as literal — could be an enum or HTTP code echo.
			in:   "/api/orders/12345",
			want: "/api/orders/12345"},
		{name: "epoch ms",
			in:   "/api/events/1717590000000",
			want: "/api/events/{id}"},

		// Long alphanumeric (nanoid/cuid/ulid/hash).
		{name: "cuid",
			in:   "/api/users/clkz0o6h00000mc1234567890",
			want: "/api/users/{id}"},
		{name: "nanoid",
			in:   "/api/notifications/V1StGXR8_Z5jdHi6B-myT",
			want: "/api/notifications/{id}"},
		{name: "ulid",
			in:   "/api/events/01ARZ3NDEKTSV4RRFFQ69G5FAV",
			want: "/api/events/{id}"},

		// Short mixed alphanumeric (8-15 chars).
		{name: "short id with digits",
			in:   "/api/holdings/abc12345",
			want: "/api/holdings/{id}"},
		{name: "short verb (no digits)", // 'overview' is 8 chars, no digits.
			in:   "/api/companies/overview",
			want: "/api/companies/overview"},

		// Ticker symbols are kept (1-5 uppercase letters, no digits).
		{name: "ticker AAPL",
			in:   "/api/symbols/AAPL",
			want: "/api/symbols/AAPL"},
		{name: "ticker BABA",
			in:   "/api/symbols/BABA",
			want: "/api/symbols/BABA"},

		// :action verbs (Go 1.22 ServeMux uses these).
		{name: "subaction with colon",
			in:   "/api/companies/abcd1234/funds:assist",
			want: "/api/companies/{id}/funds:assist"},

		// Trailing slash preserved.
		{name: "trailing slash",
			in:   "/api/funds/550e8400-e29b-41d4-a716-446655440000/",
			want: "/api/funds/{id}/"},

		// Snake-case literal verbs do NOT collapse.
		{name: "snake_case verb",
			in:   "/api/admin/decision_input_fingerprint",
			want: "/api/admin/decision_input_fingerprint"},

		// Mixed: deep templatization.
		{name: "decision drill",
			in:   "/api/funds/550e8400-e29b-41d4-a716-446655440000/decisions/01HX1234567890ABCDEFGHJK",
			want: "/api/funds/{id}/decisions/{id}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := templatizeAPIPath(tc.in)
			if got != tc.want {
				t.Fatalf("templatizeAPIPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsLikelyOpaqueIDFalseNegatives(t *testing.T) {
	// These should all NOT collapse — they're known route literals.
	keep := []string{
		"funds", "plans", "decisions", "auth", "login", "logout", "register",
		"health", "metrics", "version", "session", "kyc", "v1", "v2", "live",
		"shadow", "approved", "rejected", "active", "superseded", "rolled_back",
		"AAPL", "MSFT", "BABA", "BTC", "ETH",
		"overview", "session", "snapshot", // 7-8 chars, no digits.
	}
	for _, s := range keep {
		if isLikelyOpaqueID(s) {
			t.Errorf("isLikelyOpaqueID(%q) = true, expected false (would collapse a route literal)", s)
		}
	}
}

func TestIsLikelyOpaqueIDTruePositives(t *testing.T) {
	collapse := []string{
		// UUIDs.
		"550e8400-e29b-41d4-a716-446655440000",
		"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
		// Long alphanumeric.
		"clkz0o6h00000mc1234567890",
		"V1StGXR8_Z5jdHi6B-myT",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",
		// Numeric ≥ 6.
		"123456",
		"1717590000000",
		// Short mixed.
		"abc12345",
		"x9y8z7w6",
	}
	for _, s := range collapse {
		if !isLikelyOpaqueID(s) {
			t.Errorf("isLikelyOpaqueID(%q) = false, expected true", s)
		}
	}
}

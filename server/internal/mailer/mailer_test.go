package mailer

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSendEmailVerificationFillsCodeAndExpiry(t *testing.T) {
	rec := &Recorder{}
	cfg := Config{BrandName: "FundAI Test"}
	payload := EmailVerificationPayload{
		To:          "user@example.com",
		DisplayName: "测试用户",
		Code:        "424242",
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}

	if err := SendEmailVerification(context.Background(), rec, cfg, payload); err != nil {
		t.Fatalf("send verification: %v", err)
	}

	got, ok := rec.Last()
	if !ok {
		t.Fatal("expected at least one recorded message")
	}
	if got.To != payload.To {
		t.Fatalf("expected To=%s, got %s", payload.To, got.To)
	}
	if !strings.Contains(got.HTMLBody, payload.Code) {
		t.Errorf("HTML body missing code: %s", got.HTMLBody)
	}
	if !strings.Contains(got.TextBody, payload.Code) {
		t.Errorf("text body missing code: %s", got.TextBody)
	}
	if !strings.Contains(got.HTMLBody, "测试用户") {
		t.Errorf("HTML body missing display name: %s", got.HTMLBody)
	}
	if !strings.Contains(got.HTMLBody, "Email verification") {
		t.Errorf("HTML body missing english header: %s", got.HTMLBody)
	}
}

func TestSendPasswordResetIncludesLink(t *testing.T) {
	rec := &Recorder{}
	cfg := Config{BrandName: "FundAI"}
	link := "https://app.example.com/reset?token=abcdef"
	payload := PasswordResetPayload{
		To:          "owner@example.com",
		DisplayName: "Alice",
		Link:        link,
		ExpiresAt:   time.Now().Add(2 * time.Hour),
	}

	if err := SendPasswordReset(context.Background(), rec, cfg, payload); err != nil {
		t.Fatalf("send reset: %v", err)
	}

	got, _ := rec.Last()
	if !strings.Contains(got.HTMLBody, link) {
		t.Errorf("HTML body missing reset link: %s", got.HTMLBody)
	}
	if !strings.Contains(got.TextBody, link) {
		t.Errorf("text body missing reset link: %s", got.TextBody)
	}
	if !strings.Contains(got.HTMLBody, "重置密码") {
		t.Errorf("HTML body missing zh title: %s", got.HTMLBody)
	}
}

func TestConfigEnabledRequiresHostAndFrom(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("empty config should not be enabled")
	}
	if (Config{Host: "smtp.example.com"}).Enabled() {
		t.Error("missing from should not be enabled")
	}
	if !(Config{Host: "smtp.example.com", From: "noreply@example.com"}).Enabled() {
		t.Error("host+from should be enabled")
	}
}

func TestBuildMIMEContainsBothParts(t *testing.T) {
	cfg := Config{From: "noreply@example.com", FromName: "FundAI"}
	out := buildMIME(cfg, Message{
		To:       "user@example.com",
		Subject:  "Hello",
		TextBody: "plain hello",
		HTMLBody: "<p>html hello</p>",
	})
	if !strings.Contains(out, "Content-Type: text/plain") {
		t.Errorf("missing plain part: %s", out)
	}
	if !strings.Contains(out, "Content-Type: text/html") {
		t.Errorf("missing html part: %s", out)
	}
	if !strings.Contains(out, "From: ") {
		t.Errorf("missing From header: %s", out)
	}
}

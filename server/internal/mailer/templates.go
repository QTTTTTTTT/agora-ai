package mailer

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"time"
)

// EmailVerificationPayload feeds the verification template.
type EmailVerificationPayload struct {
	To          string
	DisplayName string
	Code        string
	ExpiresAt   time.Time
}

// PasswordResetPayload feeds the password-reset template.
type PasswordResetPayload struct {
	To          string
	DisplayName string
	Link        string
	ExpiresAt   time.Time
}

// PasswordChangedPayload feeds the security-notice template that fires
// after a successful password change.
type PasswordChangedPayload struct {
	To          string
	DisplayName string
	ChangedAt   time.Time
}

// SendEmailVerification renders + dispatches a verification email.
func SendEmailVerification(ctx context.Context, m Mailer, cfg Config, payload EmailVerificationPayload) error {
	if m == nil {
		return fmt.Errorf("mailer: not configured")
	}
	msg := Message{
		To:       payload.To,
		Subject:  fmt.Sprintf("[%s] 邮箱验证码 / Email verification code", brandOrDefault(cfg)),
		HTMLBody: renderHTML(cfg, verificationHTMLZH(payload), verificationHTMLEN(payload)),
		TextBody: renderText(verificationTextZH(payload), verificationTextEN(payload)),
	}
	return m.Send(ctx, msg)
}

// SendPasswordReset renders + dispatches a reset-link email.
func SendPasswordReset(ctx context.Context, m Mailer, cfg Config, payload PasswordResetPayload) error {
	if m == nil {
		return fmt.Errorf("mailer: not configured")
	}
	msg := Message{
		To:       payload.To,
		Subject:  fmt.Sprintf("[%s] 重置密码 / Password reset", brandOrDefault(cfg)),
		HTMLBody: renderHTML(cfg, resetHTMLZH(payload), resetHTMLEN(payload)),
		TextBody: renderText(resetTextZH(payload), resetTextEN(payload)),
	}
	return m.Send(ctx, msg)
}

// SendPasswordChangedNotice tells a user their password was rotated.
func SendPasswordChangedNotice(ctx context.Context, m Mailer, cfg Config, payload PasswordChangedPayload) error {
	if m == nil {
		return fmt.Errorf("mailer: not configured")
	}
	msg := Message{
		To:       payload.To,
		Subject:  fmt.Sprintf("[%s] 密码已更新 / Password changed", brandOrDefault(cfg)),
		HTMLBody: renderHTML(cfg, changedHTMLZH(payload), changedHTMLEN(payload)),
		TextBody: renderText(changedTextZH(payload), changedTextEN(payload)),
	}
	return m.Send(ctx, msg)
}

func brandOrDefault(cfg Config) string {
	if name := strings.TrimSpace(cfg.BrandName); name != "" {
		return name
	}
	return "FundAI"
}

func renderHTML(cfg Config, zh, en string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'PingFang SC','Microsoft YaHei',sans-serif;background:#0f172a;color:#e2e8f0;margin:0;padding:24px 0;">`)
	b.WriteString(`<div style="max-width:560px;margin:0 auto;background:#111827;border:1px solid #1f2937;border-radius:16px;padding:32px 28px;">`)
	b.WriteString(fmt.Sprintf(`<div style="font-size:13px;color:#818cf8;letter-spacing:0.12em;text-transform:uppercase;margin-bottom:18px;">%s</div>`, html.EscapeString(brandOrDefault(cfg))))
	b.WriteString(zh)
	b.WriteString(`<hr style="border:none;border-top:1px solid #1f2937;margin:28px 0;"/>`)
	b.WriteString(en)
	b.WriteString(`<p style="margin-top:32px;font-size:12px;color:#64748b;">如非本人操作，请忽略本邮件。If you did not request this, ignore this email.</p>`)
	b.WriteString(`</div></body></html>`)
	return b.String()
}

func renderText(zh, en string) string {
	return zh + "\n\n----\n\n" + en + "\n\n如非本人操作，请忽略本邮件。 / If you did not request this, ignore this email.\n"
}

func verificationHTMLZH(p EmailVerificationPayload) string {
	greet := html.EscapeString(displayOrDefault(p.DisplayName))
	return fmt.Sprintf(`
<h2 style="font-size:20px;color:#f8fafc;margin:0 0 12px 0;">邮箱验证码</h2>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 16px 0;">%s 你好，</p>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 18px 0;">请使用以下 6 位验证码完成邮箱验证：</p>
<div style="font-size:32px;letter-spacing:0.32em;font-weight:600;color:#fbbf24;background:#1f2937;border-radius:12px;padding:18px 0;text-align:center;margin:0 0 14px 0;">%s</div>
<p style="font-size:13px;color:#94a3b8;margin:0;">验证码将在 %s 过期。</p>`,
		greet,
		html.EscapeString(p.Code),
		html.EscapeString(formatRemaining(p.ExpiresAt, "zh")),
	)
}

func verificationHTMLEN(p EmailVerificationPayload) string {
	greet := html.EscapeString(displayOrDefault(p.DisplayName))
	return fmt.Sprintf(`
<h2 style="font-size:20px;color:#f8fafc;margin:0 0 12px 0;">Email verification</h2>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 16px 0;">Hi %s,</p>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 18px 0;">Use the 6-digit code below to verify your email address:</p>
<div style="font-size:32px;letter-spacing:0.32em;font-weight:600;color:#fbbf24;background:#1f2937;border-radius:12px;padding:18px 0;text-align:center;margin:0 0 14px 0;">%s</div>
<p style="font-size:13px;color:#94a3b8;margin:0;">The code expires in %s.</p>`,
		greet,
		html.EscapeString(p.Code),
		html.EscapeString(formatRemaining(p.ExpiresAt, "en")),
	)
}

func verificationTextZH(p EmailVerificationPayload) string {
	return fmt.Sprintf("%s 你好，\n\n请使用以下验证码完成邮箱验证：%s\n验证码将在 %s 过期。",
		displayOrDefault(p.DisplayName),
		p.Code,
		formatRemaining(p.ExpiresAt, "zh"),
	)
}

func verificationTextEN(p EmailVerificationPayload) string {
	return fmt.Sprintf("Hi %s,\n\nUse this code to verify your email: %s\nThe code expires in %s.",
		displayOrDefault(p.DisplayName),
		p.Code,
		formatRemaining(p.ExpiresAt, "en"),
	)
}

func resetHTMLZH(p PasswordResetPayload) string {
	greet := html.EscapeString(displayOrDefault(p.DisplayName))
	return fmt.Sprintf(`
<h2 style="font-size:20px;color:#f8fafc;margin:0 0 12px 0;">重置密码</h2>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 16px 0;">%s 你好，</p>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 18px 0;">点击下方按钮即可重置密码。链接将在 %s 后过期。</p>
<p style="margin:0 0 18px 0;"><a href="%s" style="display:inline-block;background:#6366f1;color:#ffffff;text-decoration:none;font-size:14px;font-weight:600;padding:12px 22px;border-radius:10px;">立即重置密码</a></p>
<p style="font-size:13px;color:#94a3b8;margin:0;">如按钮无法点击，请将以下链接复制到浏览器：<br/><span style="color:#a5b4fc;word-break:break-all;">%s</span></p>`,
		greet,
		html.EscapeString(formatRemaining(p.ExpiresAt, "zh")),
		html.EscapeString(p.Link),
		html.EscapeString(p.Link),
	)
}

func resetHTMLEN(p PasswordResetPayload) string {
	greet := html.EscapeString(displayOrDefault(p.DisplayName))
	return fmt.Sprintf(`
<h2 style="font-size:20px;color:#f8fafc;margin:0 0 12px 0;">Password reset</h2>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 16px 0;">Hi %s,</p>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 18px 0;">Click the button below to choose a new password. The link expires in %s.</p>
<p style="margin:0 0 18px 0;"><a href="%s" style="display:inline-block;background:#6366f1;color:#ffffff;text-decoration:none;font-size:14px;font-weight:600;padding:12px 22px;border-radius:10px;">Reset my password</a></p>
<p style="font-size:13px;color:#94a3b8;margin:0;">If the button does not work, paste this URL into your browser:<br/><span style="color:#a5b4fc;word-break:break-all;">%s</span></p>`,
		greet,
		html.EscapeString(formatRemaining(p.ExpiresAt, "en")),
		html.EscapeString(p.Link),
		html.EscapeString(p.Link),
	)
}

func resetTextZH(p PasswordResetPayload) string {
	return fmt.Sprintf("%s 你好，\n\n请点击以下链接重置密码（%s 后过期）：\n%s",
		displayOrDefault(p.DisplayName),
		formatRemaining(p.ExpiresAt, "zh"),
		p.Link,
	)
}

func resetTextEN(p PasswordResetPayload) string {
	return fmt.Sprintf("Hi %s,\n\nUse this link to reset your password (expires in %s):\n%s",
		displayOrDefault(p.DisplayName),
		formatRemaining(p.ExpiresAt, "en"),
		p.Link,
	)
}

func changedHTMLZH(p PasswordChangedPayload) string {
	greet := html.EscapeString(displayOrDefault(p.DisplayName))
	return fmt.Sprintf(`
<h2 style="font-size:20px;color:#f8fafc;margin:0 0 12px 0;">密码已更新</h2>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 16px 0;">%s 你好，</p>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 14px 0;">您的账户密码已于 %s 成功更新。</p>
<p style="font-size:13px;color:#fda4af;margin:0;">如非本人操作，请立即登录控制台修改密码并联系管理员。</p>`,
		greet,
		html.EscapeString(p.ChangedAt.In(time.UTC).Format("2006-01-02 15:04 UTC")),
	)
}

func changedHTMLEN(p PasswordChangedPayload) string {
	greet := html.EscapeString(displayOrDefault(p.DisplayName))
	return fmt.Sprintf(`
<h2 style="font-size:20px;color:#f8fafc;margin:0 0 12px 0;">Password changed</h2>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 16px 0;">Hi %s,</p>
<p style="font-size:14px;line-height:1.7;color:#cbd5f5;margin:0 0 14px 0;">Your account password was changed on %s.</p>
<p style="font-size:13px;color:#fda4af;margin:0;">If you did not perform this change, sign in immediately to rotate it and contact support.</p>`,
		greet,
		html.EscapeString(p.ChangedAt.In(time.UTC).Format("2006-01-02 15:04 UTC")),
	)
}

func changedTextZH(p PasswordChangedPayload) string {
	return fmt.Sprintf("%s 你好，\n\n您的账户密码已于 %s 成功更新。如非本人操作，请立即修改密码并联系管理员。",
		displayOrDefault(p.DisplayName),
		p.ChangedAt.In(time.UTC).Format("2006-01-02 15:04 UTC"),
	)
}

func changedTextEN(p PasswordChangedPayload) string {
	return fmt.Sprintf("Hi %s,\n\nYour account password was changed on %s. If this wasn't you, sign in immediately to rotate it and contact support.",
		displayOrDefault(p.DisplayName),
		p.ChangedAt.In(time.UTC).Format("2006-01-02 15:04 UTC"),
	)
}

func displayOrDefault(name string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return "there"
}

func formatRemaining(expiresAt time.Time, lang string) string {
	if expiresAt.IsZero() {
		if lang == "zh" {
			return "15 分钟"
		}
		return "15 minutes"
	}
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		if lang == "zh" {
			return "极短时间内"
		}
		return "a very short time"
	}
	if remaining >= time.Hour {
		hours := int(remaining.Round(time.Minute).Hours())
		if lang == "zh" {
			return fmt.Sprintf("%d 小时", hours)
		}
		return fmt.Sprintf("%d hour(s)", hours)
	}
	minutes := int(remaining.Round(time.Minute).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	if lang == "zh" {
		return fmt.Sprintf("%d 分钟", minutes)
	}
	return fmt.Sprintf("%d minute(s)", minutes)
}

func buildMIME(cfg Config, msg Message) string {
	boundary := fmt.Sprintf("fundai-%d", time.Now().UnixNano())
	var buf bytes.Buffer

	headers := textproto.MIMEHeader{}
	headers.Set("From", formatFromHeader(cfg))
	headers.Set("To", msg.To)
	headers.Set("Subject", mime.BEncoding.Encode("utf-8", msg.Subject))
	headers.Set("MIME-Version", "1.0")
	headers.Set("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary))
	headers.Set("Date", time.Now().UTC().Format(time.RFC1123Z))

	for k, vs := range headers {
		for _, v := range vs {
			fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
		}
	}
	buf.WriteString("\r\n")

	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return buf.String()
	}

	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textHeader.Set("Content-Transfer-Encoding", "base64")
	if p, err := mw.CreatePart(textHeader); err == nil {
		p.Write([]byte(base64Wrap(msg.TextBody)))
	}

	htmlHeader := textproto.MIMEHeader{}
	htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
	htmlHeader.Set("Content-Transfer-Encoding", "base64")
	if p, err := mw.CreatePart(htmlHeader); err == nil {
		p.Write([]byte(base64Wrap(msg.HTMLBody)))
	}

	mw.Close()
	return buf.String()
}

func formatFromHeader(cfg Config) string {
	name := mime.BEncoding.Encode("utf-8", cfg.FromName)
	return fmt.Sprintf("%s <%s>", name, cfg.From)
}

// base64Wrap encodes the body and inserts CRLF every 76 chars
// (matching RFC 2045's recommendation for MIME-safe wrapping).
func base64Wrap(payload string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	var out strings.Builder
	const line = 76
	for i := 0; i < len(encoded); i += line {
		end := i + line
		if end > len(encoded) {
			end = len(encoded)
		}
		out.WriteString(encoded[i:end])
		out.WriteString("\r\n")
	}
	return out.String()
}

package api

import (
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	keys := strings.Fields("CLOUDFRONT_URL CLOUDFRONT_KEY_ID CLOUDFRONT_PRIVATE_KEY STORAGE_BUCKET AWS_REGION DB_HOST DB_PORT DB_USER DB_PASSWORD DB_NAME DB_SSLMODE DB_SSLROOTCERT DB_MAX_CONNS MAX_CONCURRENT_REQUESTS AUTO_MIGRATE TRUSTED_PROXY_CIDRS DATABASE_URL HTTP_ADDR STORAGE_DIR SIGNING_KEY PUBLIC_URL SITE_URL CORS_ORIGINS ANTHROPIC_API_KEY ANTHROPIC_MODEL FACTS_MODEL CHAT_MODEL SEARCH_MODEL SMTP_HOST SMTP_PORT SMTP_USER SMTP_PASSWORD SMTP_FROM DEV_AUTO_CONFIRM STORAGE_QUOTA_BYTES GOOGLE_AUTH_ENABLED GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET GOOGLE_REDIRECT_URL GOOGLE_RETURN_URLS ANTHROPIC_EFFORT CHAT_MAX_TOKENS SMTP_MIN_INTERVAL_SECONDS")
	for _, tc := range []struct {
		name      string
		env       map[string]string
		wantError bool
	}{
		{"local defaults", nil, false},
		{"missing database", map[string]string{"DATABASE_URL": ""}, true},
		{"short signing key", map[string]string{"SIGNING_KEY": "short"}, true},
		{"production requires SMTP", map[string]string{"DEV_AUTO_CONFIRM": "false"}, true},
		{"missing sender", map[string]string{"DEV_AUTO_CONFIRM": "false", "SMTP_HOST": "smtp.example.invalid"}, true},
		{"configured SMTP", map[string]string{"DEV_AUTO_CONFIRM": "false", "SMTP_HOST": "smtp.example.invalid", "SMTP_FROM": "Broto <noreply@example.invalid>"}, false},
		{"bad port", map[string]string{"SMTP_HOST": "smtp.example.invalid", "SMTP_FROM": "a@example.invalid", "SMTP_PORT": "not-a-port"}, true},
		{"port range", map[string]string{"SMTP_HOST": "smtp.example.invalid", "SMTP_FROM": "a@example.invalid", "SMTP_PORT": "65536"}, true},
		{"bad sender", map[string]string{"SMTP_HOST": "smtp.example.invalid", "SMTP_FROM": "broken"}, true},
		{"zero quota", map[string]string{"STORAGE_QUOTA_BYTES": "0"}, true},
		{"negative quota", map[string]string{"STORAGE_QUOTA_BYTES": "-1"}, true},
		{"invalid quota", map[string]string{"STORAGE_QUOTA_BYTES": "abc"}, true},
		{"custom quota", map[string]string{"STORAGE_QUOTA_BYTES": "4096"}, false},
		{"custom address", map[string]string{"HTTP_ADDR": "127.0.0.1:8081"}, false},
		{"Google requires credentials", map[string]string{"GOOGLE_AUTH_ENABLED": "true"}, true},
		{"invalid effort", map[string]string{"ANTHROPIC_EFFORT": "unlimited"}, true},
		{"valid effort", map[string]string{"ANTHROPIC_EFFORT": "medium"}, false},
		{"invalid email interval", map[string]string{"SMTP_MIN_INTERVAL_SECONDS": "0"}, true},
		{"invalid chat budget", map[string]string{"CHAT_MAX_TOKENS": "0"}, true},
		{"Google only without SMTP", map[string]string{"GOOGLE_AUTH_ENABLED": "true", "DEV_AUTO_CONFIRM": "false", "GOOGLE_CLIENT_ID": "test.apps.googleusercontent.com", "GOOGLE_CLIENT_SECRET": "test-secret"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, "")
			}
			t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
			t.Setenv("SIGNING_KEY", strings.Repeat("x", 32))
			t.Setenv("DEV_AUTO_CONFIRM", "true")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, e := LoadConfig()
			if (e != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", e, tc.wantError)
			}
			if tc.name == "local defaults" && (c.StorageQuota != 512<<20 || c.SMTPPort != "587" || c.Address != ":8080" || !c.DevAuth) {
				t.Fatal("incorrect defaults")
			}
			if tc.name == "custom quota" && c.StorageQuota != 4096 {
				t.Fatal(c.StorageQuota)
			}
			if tc.name == "custom address" && c.Address != "127.0.0.1:8081" {
				t.Fatal(c.Address)
			}
		})
	}
}

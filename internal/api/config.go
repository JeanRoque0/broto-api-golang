package api

import (
	"errors"
	"net/mail"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	CDNURL, CDNKeyID, CDNPrivateKey                                           string
	StorageBucket, AWSRegion                                                  string
	DBMaxConns, MaxRequests                                                   int
	DisableMigrations                                                         bool
	TrustedProxyCIDRs                                                         string
	ChatMaxTokens, SMTPMinIntervalSeconds                                     int
	DatabaseURL, Address, StorageDir, SigningKey, PublicURL, SiteURL, Origins string
	DeepSeekKey, VisionModel, FactsModel, ChatModel, SearchModel              string
	SMTPHost, SMTPPort, SMTPUser, SMTPPassword, SMTPFrom                      string
	DevAuth                                                                   bool
	StorageQuota                                                              int64
	GoogleEnabled                                                             bool
	GoogleClientID, GoogleClientSecret, GoogleRedirectURL, GoogleReturnURLs   string
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func LoadConfig() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), Address: env("HTTP_ADDR", ":8080"), StorageDir: env("STORAGE_DIR", "data/photos"), SigningKey: os.Getenv("SIGNING_KEY"), PublicURL: env("PUBLIC_URL", "http://localhost:8080"), SiteURL: env("SITE_URL", "http://localhost:3000"), Origins: env("CORS_ORIGINS", "http://localhost:3000"), DeepSeekKey: os.Getenv("DEEPSEEK_API_KEY"), VisionModel: env("VISION_MODEL", "deepseek-flash"), FactsModel: env("TEXT_MODEL", "deepseek-flash"), ChatModel: env("CHAT_MODEL", "deepseek-flash"), SearchModel: env("SEARCH_MODEL", "deepseek-flash"), SMTPHost: os.Getenv("SMTP_HOST"), SMTPPort: env("SMTP_PORT", "587"), SMTPUser: os.Getenv("SMTP_USER"), SMTPPassword: os.Getenv("SMTP_PASSWORD"), SMTPFrom: os.Getenv("SMTP_FROM"), DevAuth: os.Getenv("DEV_AUTO_CONFIRM") == "true"}
	c.StorageQuota = 512 << 20
	var dbError error
	c.DatabaseURL, dbError = databaseURL()
	if dbError != nil {
		return c, dbError
	}
	c.CDNURL = os.Getenv("CLOUDFRONT_URL")
	c.CDNKeyID = os.Getenv("CLOUDFRONT_KEY_ID")
	c.CDNPrivateKey = os.Getenv("CLOUDFRONT_PRIVATE_KEY")
	c.StorageBucket = os.Getenv("STORAGE_BUCKET")
	if c.CDNURL != "" && c.StorageBucket == "" {
		return c, errors.New("CloudFront requires S3 storage")
	}
	c.AWSRegion = os.Getenv("AWS_REGION")
	c.DisableMigrations = env("AUTO_MIGRATE", "true") == "false"
	c.TrustedProxyCIDRs = os.Getenv("TRUSTED_PROXY_CIDRS")
	if _, e := parseTrustedProxies(c.TrustedProxyCIDRs); e != nil {
		return c, e
	}
	c.DBMaxConns, dbError = strconv.Atoi(env("DB_MAX_CONNS", "10"))
	if dbError != nil || c.DBMaxConns < 2 || c.DBMaxConns > 50 {
		return c, errors.New("invalid DB_MAX_CONNS (2..50)")
	}
	c.MaxRequests, dbError = strconv.Atoi(env("MAX_CONCURRENT_REQUESTS", "24"))
	if dbError != nil || c.MaxRequests < 1 || c.MaxRequests > 128 {
		return c, errors.New("invalid MAX_CONCURRENT_REQUESTS (1..128)")
	}
	if c.StorageBucket != "" && c.AWSRegion == "" {
		return c, errors.New("AWS_REGION is required for S3 storage")
	}
	var configError error
	c.SMTPMinIntervalSeconds, configError = strconv.Atoi(env("SMTP_MIN_INTERVAL_SECONDS", "60"))
	if configError != nil || c.SMTPMinIntervalSeconds < 1 || c.SMTPMinIntervalSeconds > 86400 {
		return c, errors.New("SMTP_MIN_INTERVAL_SECONDS must be between 1 and 86400")
	}
	c.ChatMaxTokens, configError = strconv.Atoi(env("CHAT_MAX_TOKENS", "600"))
	if configError != nil || c.ChatMaxTokens < 128 || c.ChatMaxTokens > 8192 {
		return c, errors.New("CHAT_MAX_TOKENS must be between 128 and 8192")
	}
	c.GoogleEnabled = os.Getenv("GOOGLE_AUTH_ENABLED") == "true"
	c.GoogleClientID = os.Getenv("GOOGLE_CLIENT_ID")
	c.GoogleClientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
	c.GoogleRedirectURL = env("GOOGLE_REDIRECT_URL", strings.TrimRight(c.PublicURL, "/")+"/v1/auth/google/callback")
	c.GoogleReturnURLs = env("GOOGLE_RETURN_URLS", "broto://auth-callback")
	if e := validateGoogleConfig(c); e != nil {
		return c, e
	}
	if v := os.Getenv("STORAGE_QUOTA_BYTES"); v != "" {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n <= 0 {
			return c, errors.New("invalid STORAGE_QUOTA_BYTES")
		}
		c.StorageQuota = n
	}
	if c.DatabaseURL == "" || len(c.SigningKey) < 32 {
		return c, errors.New("DATABASE_URL and SIGNING_KEY (at least 32 characters) are required")
	}
	if !c.DevAuth && !c.GoogleEnabled && (c.SMTPHost == "" || c.SMTPFrom == "") {
		return c, errors.New("configure SMTP_HOST/SMTP_FROM or explicitly enable DEV_AUTO_CONFIRM for local development")
	}
	if c.SMTPHost != "" {
		port, e := strconv.Atoi(c.SMTPPort)
		if e != nil || port < 1 || port > 65535 {
			return c, errors.New("invalid SMTP_PORT")
		}
		if _, e := mail.ParseAddress(c.SMTPFrom); e != nil {
			return c, errors.New("invalid SMTP_FROM")
		}
	}
	return c, nil
}

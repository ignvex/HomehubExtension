package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/robfig/cron/v3"
)

// JobNames 是 scheduler 认识的全部清理任务。新增任务必须同时改这里、
// docs/config.md 和 scheduler 的注册表，三处不一致时启动即报错。
var JobNames = []string{
	"paste-expired",
	"session-expired",
	"rss-prune",
	"media-retry",
	"blob-gc",
	"db-vacuum",
	"job-log-prune",
}

func knownJob(name string) bool {
	for _, n := range JobNames {
		if n == name {
			return true
		}
	}
	return false
}

// Validate 一次性收集所有问题，而不是撞到第一个就返回。
// 部署时只看一次日志就能把配置改完。
func (c *Config) Validate() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Version != 1 {
		add("version: unsupported value %d (only 1 is accepted)", c.Version)
	}

	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateAuth()...)
	errs = append(errs, c.validateDatabase()...)
	errs = append(errs, c.validateStorage()...)
	errs = append(errs, c.validatePastebin()...)
	errs = append(errs, c.validateRSS()...)
	errs = append(errs, c.validateCleanup()...)
	errs = append(errs, c.validateLog()...)

	return errs
}

func (c *Config) validateServer() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Server.Addr == "" {
		add("server.addr: must not be empty")
	} else if host, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		add("server.addr: must be host:port, got %q", c.Server.Addr)
	} else if !c.Server.AllowRemoteListen && !isLoopbackHost(host) {
		add("server.addr: %q is not a loopback address; set server.allow_remote_listen=true to allow it", host)
	}

	if c.Server.PublicURL != "" {
		u, err := url.Parse(c.Server.PublicURL)
		switch {
		case err != nil:
			add("server.public_url: %v", err)
		case u.Scheme != "http" && u.Scheme != "https":
			add("server.public_url: scheme must be http or https, got %q", u.Scheme)
		case u.Host == "":
			add("server.public_url: missing host")
		case strings.HasSuffix(c.Server.PublicURL, "/"):
			add("server.public_url: must not end with a slash")
		}
	}

	if c.Server.MaxBodyBytes < 1024 {
		add("server.max_body_bytes: must be >= 1024 (got %d)", c.Server.MaxBodyBytes)
	}

	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil && net.ParseIP(cidr) == nil {
			add("server.trusted_proxies: invalid CIDR %q", cidr)
		}
	}
	return errs
}

func (c *Config) validateAuth() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if !c.Auth.Enabled {
		return nil
	}

	// Homehub 入站通道是可选的：没有配置公钥时该通道不会注册，
	// 而不是拒绝启动。很多部署一开始只用浏览器访问。
	if c.Auth.SessionTTL.Duration <= 0 {
		add("auth.session_ttl: must be positive")
	}
	if c.Auth.ChallengeTTL.Duration <= 0 {
		add("auth.challenge_ttl: must be positive")
	}
	if c.Auth.FailedAttemptLimit <= 0 {
		add("auth.failed_attempt_limit: must be positive")
	}
	return errs
}

func (c *Config) validateDatabase() []error {
	var errs []error
	if c.Database.Path == "" {
		errs = append(errs, fmt.Errorf("database.path: must not be empty"))
	}
	if c.Database.ReadConns < 1 || c.Database.ReadConns > 8 {
		errs = append(errs, fmt.Errorf("database.read_conns: must be between 1 and 8 (got %d)", c.Database.ReadConns))
	}
	return errs
}

func (c *Config) validateStorage() []error {
	var errs []error
	if c.Storage.BlobDir == "" {
		errs = append(errs, fmt.Errorf("storage.blob_dir: must not be empty"))
	}
	if c.Storage.MaxBlobBytes <= 0 {
		errs = append(errs, fmt.Errorf("storage.max_blob_bytes: must be positive"))
	}
	if c.Storage.QuotaBytes > 0 && c.Storage.MaxBlobBytes > c.Storage.QuotaBytes {
		errs = append(errs, fmt.Errorf("storage.max_blob_bytes: must not exceed storage.quota_bytes"))
	}
	return errs
}

func (c *Config) validatePastebin() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if !c.Pastebin.Enabled {
		return nil
	}

	if c.Pastebin.MaxBodyBytes <= 0 {
		add("pastebin.max_body_bytes: must be positive")
	}
	if c.Pastebin.MaxBodyBytes > c.Server.MaxBodyBytes {
		add("pastebin.max_body_bytes: must not exceed server.max_body_bytes")
	}
	if c.Pastebin.SlugLength < 4 || c.Pastebin.SlugLength > 12 {
		add("pastebin.slug_length: must be between 4 and 12 (got %d)", c.Pastebin.SlugLength)
	}
	if c.Pastebin.MaxExpiry.Duration > 0 && c.Pastebin.DefaultExpiry.Duration > c.Pastebin.MaxExpiry.Duration {
		add("pastebin.default_expiry: must not exceed pastebin.max_expiry")
	}
	if len(c.Pastebin.LanguageList) == 0 {
		add("pastebin.language_list: must not be empty")
	}
	return errs
}

func (c *Config) validateRSS() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if !c.RSS.Enabled {
		return nil
	}

	if c.RSS.FetchConcurrency < 1 || c.RSS.FetchConcurrency > 8 {
		add("rss.fetch_concurrency: must be between 1 and 8 (got %d)", c.RSS.FetchConcurrency)
	}
	if c.RSS.MediaConcurrency < 1 || c.RSS.MediaConcurrency > 8 {
		add("rss.media_concurrency: must be between 1 and 8 (got %d)", c.RSS.MediaConcurrency)
	}
	if c.RSS.MinInterval.Duration <= 0 {
		add("rss.min_interval: must be positive")
	}
	if c.RSS.DefaultInterval.Duration < c.RSS.MinInterval.Duration {
		add("rss.default_interval: must be >= rss.min_interval")
	}
	if c.RSS.MaxInterval.Duration < c.RSS.DefaultInterval.Duration {
		add("rss.max_interval: must be >= rss.default_interval")
	}
	switch c.RSS.Referer {
	case "auto", "site", "none":
	default:
		add("rss.referer: must be one of auto, site, none (got %q)", c.RSS.Referer)
	}
	if c.RSS.UserAgent == "" {
		add("rss.user_agent: must not be empty")
	}
	return errs
}

func (c *Config) validateLog() []error {
	var errs []error
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: must be one of debug, info, warn, error (got %q)", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format: must be json or text (got %q)", c.Log.Format))
	}
	return errs
}

func (c *Config) validateCleanup() []error {
	var errs []error
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

	seen := map[string]bool{}
	for i, job := range c.Cleanup.Jobs {
		prefix := fmt.Sprintf("cleanup.jobs[%d]", i)
		if job.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name: must not be empty", prefix))
		} else if !knownJob(job.Name) {
			errs = append(errs, fmt.Errorf("%s.name: unknown job %q (known: %s)",
				prefix, job.Name, strings.Join(JobNames, ", ")))
		} else if seen[job.Name] {
			errs = append(errs, fmt.Errorf("%s.name: duplicate job %q", prefix, job.Name))
		}
		seen[job.Name] = true

		if job.Schedule == "" {
			errs = append(errs, fmt.Errorf("%s.schedule: must not be empty", prefix))
		} else if _, err := parser.Parse(job.Schedule); err != nil {
			errs = append(errs, fmt.Errorf("%s.schedule: invalid cron expression %q: %v", prefix, job.Schedule, err))
		}
		if job.Timeout.Duration < 0 {
			errs = append(errs, fmt.Errorf("%s.timeout: must not be negative", prefix))
		}
	}
	if c.Cleanup.Timezone != "" && c.Cleanup.Timezone != "Local" {
		if _, err := loadTimezone(c.Cleanup.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("cleanup.timezone: unknown time zone %q", c.Cleanup.Timezone))
		}
	}
	return errs
}

// Redacted 返回用于 /api/v1/system/config 的脱敏视图。
// 密钥类字段一律不出现在响应里，也不返回"已设置"之外的任何信息。
func (c Config) Redacted() map[string]any {
	cp := c
	cp.Auth.ServerKeyPath = redactPath(cp.Auth.ServerKeyPath)
	cp.Auth.Hub.PublicKeyPath = redactPath(cp.Auth.Hub.PublicKeyPath)
	return map[string]any{
		"version":  cp.Version,
		"server":   cp.Server,
		"auth":     cp.Auth,
		"database": map[string]any{"path": cp.Database.Path, "read_conns": cp.Database.ReadConns},
		"storage":  cp.Storage,
		"pastebin": cp.Pastebin,
		"rss":      cp.RSS,
		"cleanup":  cp.Cleanup,
		"log":      cp.Log,
	}
}

func redactPath(p string) string {
	if p == "" {
		return ""
	}
	return "(configured)"
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

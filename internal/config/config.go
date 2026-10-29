package config

import "time"

// Config 是 config.json 的完整映射。字段顺序与 docs/config.md 的章节一致。
type Config struct {
	Version  int      `json:"version"`
	Server   Server   `json:"server"`
	Auth     Auth     `json:"auth"`
	Database Database `json:"database"`
	Storage  Storage  `json:"storage"`
	Pastebin Pastebin `json:"pastebin"`
	RSS      RSS      `json:"rss"`
	Cleanup  Cleanup  `json:"cleanup"`
	Log      Log      `json:"log"`
}

// Server 是 HTTP 监听与反向代理相关配置。
type Server struct {
	Addr              string   `json:"addr"`
	PublicURL         string   `json:"public_url"`
	AllowRemoteListen bool     `json:"allow_remote_listen"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	TrustedProxies    []string `json:"trusted_proxies"`
	MaxBodyBytes      int64    `json:"max_body_bytes"`
}

// Auth 是浏览器侧鉴权与宿主入站签名配置。
type Auth struct {
	Enabled             bool     `json:"enabled"`
	ServerKeyPath       string   `json:"server_key_path"`
	SessionTTL          Duration `json:"session_ttl"`
	ChallengeTTL        Duration `json:"challenge_ttl"`
	EnrollmentTTL       Duration `json:"enrollment_ttl"`
	FailedAttemptLimit  int      `json:"failed_attempt_limit"`
	FailedAttemptWindow Duration `json:"failed_attempt_window"`
	// Hub 是 Homehub 宿主推送订阅时使用的入站签名密钥。
	Hub HubAuth `json:"hub"`
}

// HubAuth 定义 Homehub → Extension 的入站鉴权。
// 这是"双向"里的第二个方向：Extension 不信任任何未经签名的推送。
type HubAuth struct {
	Enabled       bool     `json:"enabled"`
	PublicKeyPath string   `json:"public_key_path"`
	ClockSkew     Duration `json:"clock_skew"`
	NonceTTL      Duration `json:"nonce_ttl"`
}

// Database 是 SQLite 路径与连接池配置。
type Database struct {
	Path        string   `json:"path"`
	BusyTimeout Duration `json:"busy_timeout"`
	ReadConns   int      `json:"read_conns"`
}

// Storage 是内容寻址文件存储的配额与目录配置。
type Storage struct {
	BlobDir      string   `json:"blob_dir"`
	TmpDir       string   `json:"tmp_dir"`
	MaxBlobBytes int64    `json:"max_blob_bytes"`
	QuotaBytes   int64    `json:"quota_bytes"`
	MinFreeBytes int64    `json:"min_free_bytes"`
	GCGrace      Duration `json:"gc_grace"`
}

// Pastebin 是片段模块的限额与语言白名单。
type Pastebin struct {
	Enabled       bool     `json:"enabled"`
	MaxBodyBytes  int64    `json:"max_body_bytes"`
	DefaultExpiry Duration `json:"default_expiry"`
	MaxExpiry     Duration `json:"max_expiry"`
	SlugLength    int      `json:"slug_length"`
	AllowPublic   bool     `json:"allow_public"`
	LanguageList  []string `json:"language_list"`
}

// RSS 是抓取、并发与 SSRF 策略配置。
type RSS struct {
	Enabled           bool     `json:"enabled"`
	DefaultInterval   Duration `json:"default_interval"`
	MinInterval       Duration `json:"min_interval"`
	MaxInterval       Duration `json:"max_interval"`
	FetchConcurrency  int      `json:"fetch_concurrency"`
	MediaConcurrency  int      `json:"media_concurrency"`
	FetchTimeout      Duration `json:"fetch_timeout"`
	RequestTimeout    Duration `json:"request_timeout"`
	UserAgent         string   `json:"user_agent"`
	MaxFeedBytes      int64    `json:"max_feed_bytes"`
	MaxEntryBytes     int64    `json:"max_entry_bytes"`
	AllowPrivateHosts bool     `json:"allow_private_hosts"`
	Referer           string   `json:"referer"`
}

// Cleanup 是全部保留期与回收策略的唯一来源。
type Cleanup struct {
	Timezone string     `json:"timezone"`
	CatchUp  bool       `json:"catch_up"`
	Jobs     []CleanJob `json:"jobs"`
}

// CleanJob 是一条定时清理任务的配置。Enabled 为 nil 表示启用。
type CleanJob struct {
	Name     string         `json:"name"`
	Schedule string         `json:"schedule"`
	Timeout  Duration       `json:"timeout"`
	Enabled  *bool          `json:"enabled"`
	Params   map[string]any `json:"params"`
}

// IsEnabled 把三态（缺省/true/false）收敛成默认启用。
func (j CleanJob) IsEnabled() bool { return j.Enabled == nil || *j.Enabled }

// Log 控制日志级别、格式与访问日志开关。
type Log struct {
	Level     string `json:"level"`
	Format    string `json:"format"`
	AccessLog bool   `json:"access_log"`
}

// Default 返回一份"文件里写了但省略了某些字段"时用于补齐的基线。
// 它永远不单独使用：config.json 必须存在。
func Default() Config {
	return Config{
		Version: 1,
		Server: Server{
			Addr:              "127.0.0.1:8080",
			ReadHeaderTimeout: Duration{5 * time.Second},
			IdleTimeout:       Duration{90 * time.Second},
			TrustedProxies:    []string{"127.0.0.1/32", "::1/128"},
			MaxBodyBytes:      1 << 20,
		},
		Auth: Auth{
			Enabled:             true,
			SessionTTL:          Duration{168 * time.Hour},
			ChallengeTTL:        Duration{2 * time.Minute},
			EnrollmentTTL:       Duration{15 * time.Minute},
			FailedAttemptLimit:  10,
			FailedAttemptWindow: Duration{5 * time.Minute},
			Hub: HubAuth{
				Enabled:   true,
				ClockSkew: Duration{60 * time.Second},
				NonceTTL:  Duration{2 * time.Minute},
			},
		},
		Database: Database{
			BusyTimeout: Duration{5 * time.Second},
			ReadConns:   3,
		},
		Storage: Storage{
			MaxBlobBytes: 50 << 20,
			QuotaBytes:   5 << 30,
			MinFreeBytes: 1 << 30,
			GCGrace:      Duration{time.Hour},
		},
		Pastebin: Pastebin{
			Enabled:       true,
			MaxBodyBytes:  256 << 10,
			DefaultExpiry: Duration{168 * time.Hour},
			MaxExpiry:     Duration{8760 * time.Hour},
			SlugLength:    6,
			AllowPublic:   true,
			LanguageList: []string{
				"text", "bash", "caddyfile", "css", "dockerfile", "go", "html",
				"ini", "javascript", "json", "lua", "markdown", "nginx", "python",
				"rust", "shell", "sql", "toml", "typescript", "xml", "yaml",
			},
		},
		RSS: RSS{
			Enabled:          true,
			DefaultInterval:  Duration{30 * time.Minute},
			MinInterval:      Duration{10 * time.Minute},
			MaxInterval:      Duration{6 * time.Hour},
			FetchConcurrency: 2,
			MediaConcurrency: 3,
			FetchTimeout:     Duration{90 * time.Second},
			RequestTimeout:   Duration{20 * time.Second},
			UserAgent:        "HomehubExtension/0.1",
			MaxFeedBytes:     5 << 20,
			MaxEntryBytes:    2 << 20,
			Referer:          "auto",
		},
		Cleanup: Cleanup{
			Timezone: "Local",
			CatchUp:  true,
			Jobs: []CleanJob{
				{Name: "paste-expired", Schedule: "0 * * * *", Timeout: Duration{5 * time.Minute}},
				{Name: "session-expired", Schedule: "15 * * * *", Timeout: Duration{2 * time.Minute}},
				{Name: "rss-prune", Schedule: "30 4 * * *", Timeout: Duration{20 * time.Minute}},
				{Name: "blob-gc", Schedule: "0 5 * * *", Timeout: Duration{30 * time.Minute}},
			},
		},
		Log: Log{Level: "info", Format: "json", AccessLog: true},
	}
}

package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `{
  "version": 1,
  "server": {"addr": "127.0.0.1:8080"},
  "database": {"path": "%DB%"},
  "storage": {"blob_dir": "%BLOB%"}
}`

func minimal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := strings.ReplaceAll(minimalConfig, "%DB%", filepath.Join(dir, "hh.db"))
	body = strings.ReplaceAll(body, "%BLOB%", filepath.Join(dir, "blobs"))
	return writeConfig(t, body)
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, path, err := Load(minimal(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path should be absolute, got %q", path)
	}
	if cfg.Server.MaxBodyBytes != 1<<20 {
		t.Errorf("max_body_bytes default = %d, want %d", cfg.Server.MaxBodyBytes, 1<<20)
	}
	if cfg.RSS.DefaultInterval.Duration != 30*time.Minute {
		t.Errorf("default_interval = %v", cfg.RSS.DefaultInterval.Duration)
	}
	if cfg.Storage.TmpDir == "" {
		t.Error("tmp_dir should be derived from blob_dir")
	}
	if !strings.HasSuffix(cfg.Storage.TmpDir, "tmp") {
		t.Errorf("tmp_dir = %q", cfg.Storage.TmpDir)
	}
	// 数据库在同目录时，密钥应派生到同一目录而不是留在相对路径。
	if cfg.Auth.ServerKeyPath == "" || !filepath.IsAbs(cfg.Auth.ServerKeyPath) {
		t.Errorf("server_key_path = %q", cfg.Auth.ServerKeyPath)
	}
}

func TestLoadMissingExplicitPathFails(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	body := `{
	  "version": 1,
	  "server": {"addr": "127.0.0.1:8080", "max_body_byte": 1024},
	  "database": {"path": "` + filepath.Join(dir, "a.db") + `"},
	  "storage": {"blob_dir": "` + filepath.Join(dir, "b") + `"}
	}`
	_, _, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected error for unknown field (typo must not be silently ignored)")
	}
	if !strings.Contains(err.Error(), "max_body_byte") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("HH_TEST_TOKEN", "s3cret")
	out, errs := expandEnv(`{"a":"${HH_TEST_TOKEN}","b":"${HH_MISSING:-fallback}","c":"$$literal"}`)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !strings.Contains(out, "s3cret") || !strings.Contains(out, "fallback") || !strings.Contains(out, "$literal") {
		t.Errorf("expandEnv result = %s", out)
	}
}

func TestExpandEnvMissing(t *testing.T) {
	_, errs := expandEnv(`{"a":"${HH_DEFINITELY_NOT_SET}"}`)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), "HH_DEFINITELY_NOT_SET") {
		t.Errorf("error should name the variable: %v", errs[0])
	}
}

// validBase 返回一份只差被测字段就能通过校验的配置。
// 直接改 Default() 会踩上 path 为空这类与用例无关的校验错误。
func validBase(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.Database.Path = filepath.Join(dir, "hh.db")
	cfg.Storage.BlobDir = filepath.Join(dir, "blobs")
	cfg.Auth.ServerKeyPath = filepath.Join(dir, "server.key")
	return cfg
}

func TestValidateAggregatesErrors(t *testing.T) {
	cfg := validBase(t)
	cfg.Server.Addr = "0.0.0.0:8080" // 非回环且未允许
	cfg.Pastebin.SlugLength = 2      // 越界
	cfg.RSS.FetchConcurrency = 0     // 越界
	cfg.Log.Level = "verbose"        // 非法

	errs := cfg.Validate()
	// 一次性收集，而不是撞到第一个就返回。
	if len(errs) < 4 {
		t.Fatalf("expected at least 4 errors, got %d: %v", len(errs), errs)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{"allow_remote_listen", "slug_length", "fetch_concurrency", "log.level"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing error about %q in:\n%s", want, joined)
		}
	}
}

func TestValidateCronExpression(t *testing.T) {
	cfg := validBase(t)
	cfg.Cleanup.Jobs = []CleanJob{{Name: "paste-expired", Schedule: "0 25 * * *"}}
	errs := cfg.Validate()
	if len(errs) == 0 {
		t.Fatal("expected error for invalid cron")
	}
	if !strings.Contains(errs[0].Error(), "cron") {
		t.Errorf("error = %v", errs[0])
	}
}

func TestValidateDuplicateJob(t *testing.T) {
	cfg := validBase(t)
	cfg.Cleanup.Jobs = []CleanJob{
		{Name: "blob-gc", Schedule: "0 5 * * *"},
		{Name: "blob-gc", Schedule: "0 6 * * *"},
	}
	errs := cfg.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", errs)
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"5 seconds"`)); err == nil {
		t.Fatal("expected parse error")
	}
	if err := d.UnmarshalJSON([]byte(`"5s"`)); err != nil {
		t.Fatal(err)
	}
	if d.Duration != 5*time.Second {
		t.Errorf("got %v", d.Duration)
	}
	if err := d.UnmarshalJSON([]byte(`"-1h"`)); err == nil {
		t.Fatal("negative duration should be rejected")
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	cfg := validBase(t)
	cfg.Auth.ServerKeyPath = "/etc/secret/server.key"
	cfg.Auth.Hub.PublicKey = "-----BEGIN PUBLIC KEY-----\nkeep-me-private\n-----END PUBLIC KEY-----"
	out := cfg.Redacted()

	authSection, ok := out["auth"].(redactedAuth)
	if !ok {
		t.Fatalf("auth section type %T", out["auth"])
	}
	if authSection.ServerKeyPath != "(configured)" {
		t.Errorf("key path leaked: %q", authSection.ServerKeyPath)
	}
	if !authSection.Hub.PublicKeySet {
		t.Error("public_key_set should be true when a key is configured")
	}

	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal redacted config: %v", err)
	}
	if strings.Contains(string(blob), "keep-me-private") || strings.Contains(string(blob), "/etc/secret") {
		t.Fatalf("redacted view leaked a secret: %s", blob)
	}
}

func TestRedactedPublicKeySetFalse(t *testing.T) {
	out := validBase(t).Redacted()
	authSection := out["auth"].(redactedAuth)
	if authSection.Hub.PublicKeySet {
		t.Error("public_key_set should be false when no key is configured")
	}
}

// 仓库根的 config.example.json 必须能被真实解析器接受。
//
// 它是运维照着改的第一份文件，带 _comment 这类字段；解析器开着
// DisallowUnknownFields，模板里多一个未声明字段就会让新部署直接起不来。
func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("config.example.json is not loadable: %v", err)
	}
	if cfg.Auth.Hub.Comment == "" {
		t.Error("auth.hub._comment should survive parsing as documentation")
	}
	if cfg.Auth.Hub.PublicKey != "" {
		t.Errorf("example must not ship a key, got %q", cfg.Auth.Hub.PublicKey)
	}
}

// _comment 是模板里的人话说明。没声明它时 DisallowUnknownFields
// 会把它当成拼写错误，示例配置直接无法启动。
func TestLoadAcceptsHubComment(t *testing.T) {
	dir := t.TempDir()
	body := `{
	  "version": 1,
	  "server": {"addr": "127.0.0.1:8080"},
	  "auth": {"hub": {"_comment": "粘贴公钥内容，不是路径", "public_key": ""}},
	  "database": {"path": "` + filepath.Join(dir, "db") + `"},
	  "storage": {"blob_dir": "` + filepath.Join(dir, "blobs") + `"}
	}`
	if _, _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// 已删除字段必须给出迁移指引，而不是一句 unknown field。
//
// 这一条来自真实事故：升级后旧配置里还留着 public_key_path，服务在
// systemd 的 restart 循环里，日志只有 "json: unknown field"。
func TestLoadExplainsRemovedField(t *testing.T) {
	dir := t.TempDir()
	body := `{
	  "version": 1,
	  "server": {"addr": "127.0.0.1:8080"},
	  "auth": {"hub": {"enabled": true, "public_key_path": "/etc/hhe/homehub.pub"}},
	  "database": {"path": "` + filepath.Join(dir, "db") + `"},
	  "storage": {"blob_dir": "` + filepath.Join(dir, "blobs") + `"}
	}`
	_, _, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a removed field must still be rejected")
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown field") {
		t.Errorf("error should explain the migration, not just say unknown field: %v", err)
	}
	if !strings.Contains(msg, "auth.hub.public_key") || !strings.Contains(msg, "public_key_path") {
		t.Errorf("error should name both the old and the new field: %v", err)
	}
}

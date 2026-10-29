package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrNotFound 表示配置文件完全找不到。调用方据此给出可操作的提示。
var ErrNotFound = errors.New("config not found")

// SearchPaths 是未显式指定路径时的探测顺序。命中即停。
func SearchPaths(cwd string) []string {
	return []string{
		filepath.Join(cwd, "config.json"),
		"/etc/homehub-extension/config.json",
	}
}

// Load 读取并校验配置。
//
// explicit 来自 -config 或 HOMEHUB_CONFIG。只要非空，路径不存在就直接失败，
// 不继续向下探测——否则"改了生产配置、实际读的是当前目录旧文件"会成为
// 最难排查的一类事故。
func Load(explicit string) (Config, string, error) {
	path, err := resolve(explicit)
	if err != nil {
		return Config{}, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, path, fmt.Errorf("read %s: %w", path, err)
	}

	expanded, expandErrs := expandEnv(string(raw))
	if len(expandErrs) > 0 {
		return Config{}, path, joinErrs(expandErrs)
	}

	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, path, fmt.Errorf("%s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cfg.resolvePaths(filepath.Dir(abs))

	if errs := cfg.Validate(); len(errs) > 0 {
		return Config{}, abs, joinErrs(errs)
	}
	return cfg, abs, nil
}

func resolve(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config %s: %w", explicit, err)
		}
		return explicit, nil
	}
	cwd, _ := os.Getwd()
	for _, p := range SearchPaths(cwd) {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", ErrNotFound
}

// resolvePaths 把相对路径钉成绝对路径，并补齐派生的默认目录。
func (c *Config) resolvePaths(base string) {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.Database.Path = abs(c.Database.Path)
	c.Storage.BlobDir = abs(c.Storage.BlobDir)
	c.Auth.ServerKeyPath = abs(c.Auth.ServerKeyPath)
	c.Auth.Hub.PublicKeyPath = abs(c.Auth.Hub.PublicKeyPath)
	c.Storage.TmpDir = abs(c.Storage.TmpDir)

	if c.Storage.TmpDir == "" && c.Storage.BlobDir != "" {
		c.Storage.TmpDir = filepath.Join(c.Storage.BlobDir, "tmp")
	}
	if c.Auth.ServerKeyPath == "" && c.Database.Path != "" {
		c.Auth.ServerKeyPath = filepath.Join(filepath.Dir(c.Database.Path), "server.key")
	}
}

// envRe 匹配 ${VAR} 与 ${VAR:-default}，$$ 用于转义成字面量 $。
// 只接受合法的环境变量名字符，避免误吞正文里的美元符号。
var envRe = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

func expandEnv(s string) (string, []error) {
	var errs []error
	out := envRe.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		sub := envRe.FindStringSubmatch(m)
		name, def := sub[1], sub[2]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if def != "" {
			return strings.TrimPrefix(def, ":-")
		}
		errs = append(errs, fmt.Errorf("environment variable %s is not set (write ${%s:-} for an empty default)", name, name))
		return ""
	})
	return out, errs
}

func joinErrs(errs []error) error {
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(e.Error())
	}
	return errors.New(b.String())
}

package main

import (
	"context"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

// openAuthOnly 只装配 enroll 子命令需要的最小依赖：数据库 + 密钥。
// 不启动 HTTP、不启动调度器——配对码是运维动作，不该顺带拉起整个服务。
func openAuthOnly(ctx context.Context, cfg config.Config) (*sqlite.DB, *auth.Service, error) {
	db, err := sqlite.Open(ctx, cfg.Database.Path, sqlite.Options{
		BusyTimeout: cfg.Database.BusyTimeout.Duration,
		ReadConns:   cfg.Database.ReadConns,
	})
	if err != nil {
		return nil, nil, err
	}
	signer, err := auth.LoadOrCreateSigner(cfg.Auth.ServerKeyPath)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	svc := auth.NewService(db.Auth(), signer, auth.Settings{
		SessionTTL:    cfg.Auth.SessionTTL.Duration,
		ChallengeTTL:  cfg.Auth.ChallengeTTL.Duration,
		EnrollmentTTL: cfg.Auth.EnrollmentTTL.Duration,
	})
	return db, svc, nil
}

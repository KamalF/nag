package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/httpapi"
	"github.com/KamalF/nag/internal/notify"
	"github.com/KamalF/nag/internal/store"
	"github.com/KamalF/nag/web"
)

// runServe boots the server: §5.1 env checks, config load with first-boot
// default write (§5.3), store open-and-migrate (§4.2), then the §10.4 boot
// lines and the HTTP server with its §8.2 timeouts.
func runServe(stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	env, err := loadServeEnv()
	if err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	cfg, wroteDefault, err := config.Load(env.config)
	if err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	st, err := store.Open(env.db)
	if err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	defer st.Close()

	api := httpapi.New(httpapi.Options{
		Store:       st,
		Config:      cfg,
		Web:         web.Files,
		Token:       env.token,
		VAPIDPublic: env.vapidPublic,
		Log:         logger,
	})

	// §10.4 boot lines — the answer to "what is this instance actually
	// running". The subscription count joins with the push commits.
	logger.Info("build version", "version", version)
	logger.Info("listening", "addr", env.addr)
	logger.Info("database", "path", env.db)
	logger.Info("config", "path", env.config, "written_from_default", wroteDefault)
	logger.Info("timezone", "tz", cfg.General.Timezone)
	logger.Info("presets", "count", len(cfg.Presets))
	logger.Info("config version", "config_version", api.ConfigVersion())

	// context wiring for graceful shutdown is an M4 commit (§10.1)
	go notify.NewSweep(st, logger).Run(context.Background())
	server := &http.Server{
		Addr:              env.addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: httpapi.ReadHeaderTimeout * time.Second,
		ReadTimeout:       httpapi.ReadTimeout * time.Second,
		WriteTimeout:      httpapi.WriteTimeout * time.Second,
		IdleTimeout:       httpapi.IdleTimeout * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	return 0
}

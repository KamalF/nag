package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

	// the one published config: handlers and the sweep each load a snapshot
	// once per request or tick, and SIGHUP replaces it (§5.5)
	live := config.NewLive(env.config, cfg, env.vapidPublic)
	// one sender, two callers: the sweep's digest and POST /api/push/test
	// (§7.5), so there is no second copy of the §7.1 ladder to drift
	sender := notify.NewPushSender(st, logger, env.vapidPublic, env.vapidPrivate, env.vapidSubject)
	api := httpapi.New(httpapi.Options{
		Store:       st,
		Config:      live,
		Web:         web.Files,
		Token:       env.token,
		VAPIDPublic: env.vapidPublic,
		Push:        sender,
		Log:         logger,
	})

	// §10.4 boot lines — the answer to "what is this instance actually
	// running".
	logger.Info("build version", "version", version)
	logger.Info("listening", "addr", env.addr)
	logger.Info("database", "path", env.db)
	logger.Info("config", "path", env.config, "written_from_default", wroteDefault)
	logger.Info("timezone", "tz", cfg.General.Timezone)
	logger.Info("presets", "count", len(cfg.Presets))
	logger.Info("config version", "config_version", live.Load().Version)
	if n, err := st.SubscriptionCount(context.Background()); err == nil {
		logger.Info("push subscriptions", "count", n)
	}
	// §7.1: finding a retired key at startup beats three weeks of silent
	// push failures
	if n, err := st.MismatchedKeyCount(context.Background(), env.vapidPublic); err == nil && n > 0 {
		logger.Warn("subscriptions bound to a different vapid key — each browser must re-subscribe (open the app once per device)",
			"count", n)
	}

	// registered before the server starts: an unhandled SIGHUP terminates,
	// and so would a SIGTERM that arrived before shutdown was wired
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go reloadOnHangup(hup, live, logger)
	stop, cancelStop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancelStop()

	sweep := notify.NewSweep(st, logger, sender, func() int { return live.Load().General.RetentionDays })
	server := &http.Server{
		Addr:              env.addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: httpapi.ReadHeaderTimeout,
		ReadTimeout:       httpapi.ReadTimeout,
		WriteTimeout:      httpapi.WriteTimeout,
		IdleTimeout:       httpapi.IdleTimeout,
	}
	if err := serveUntil(stop, server, sweep.Run, st.Close); err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	logger.Info("shut down cleanly")
	return 0
}

// shutdownTimeout bounds http.Server.Shutdown (§10.1), well inside Docker's
// 10-second stop grace.
const shutdownTimeout = 5 * time.Second

// serveUntil runs the server and the sweep until stop is cancelled — the
// binary is PID 1 and gets no default signal handling (§10.1) — then shuts
// down in the one order that is safe: the server stops taking requests,
// the sweep's context is cancelled and the sweep is waited for, and only
// then is the database closed, so its WAL checkpoint never runs under a
// tick still writing delivery_error or deleting a subscription. The wait
// is short because every send in the sweep is a child of its context.
func serveUntil(stop context.Context, server *http.Server, sweep func(context.Context), closeDB func() error) error {
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweep(sweepCtx)
	}()
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()

	var serveErr error
	select {
	case err := <-served: // could not listen at all
		serveErr = err
	case <-stop.Done():
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			serveErr = fmt.Errorf("shutdown: %w", err)
		}
	}
	cancelSweep()
	<-sweepDone
	if err := closeDB(); err != nil && serveErr == nil {
		serveErr = fmt.Errorf("close database: %w", err)
	}
	return serveErr
}

// reloadOnHangup is the reload path: `docker compose kill -s HUP nag` (§5.5).
// The outcome exists only in the log — `nag config check` before the HUP is
// how a bad edit costs nothing.
func reloadOnHangup(hup <-chan os.Signal, live *config.Live, logger *slog.Logger) {
	for range hup {
		live.Reload(logger)
	}
}

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

// runServe boots the server. It checks the env (§5.1), loads the config and
// writes the default on first boot (§5.3), and opens and migrates the store
// (§4.2). Then it logs the §10.4 boot lines and starts the HTTP server with
// its §8.2 timeouts.
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

	// live holds the one published config. Handlers and the sweep each load
	// a snapshot of it once per request or tick. SIGHUP replaces the
	// published config (§5.5)
	live := config.NewLive(env.config, cfg, env.vapidPublic)
	// one sender, two callers: the sweep's digest and POST /api/push/test
	// (§7.5). So the §7.1 ladder exists in one copy only, and no second
	// copy can drift away from it
	sender := notify.NewPushSender(st, logger, env.vapidPublic, env.vapidPrivate, env.vapidSubject)
	api := httpapi.New(httpapi.Options{
		Store:  st,
		Config: live,
		Web:    web.Files,
		Token:  env.token,
		Push:   sender,
		Log:    logger,
	})

	// §10.4 boot lines: they answer "what is this instance actually
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
	// §7.1: it is better to find a retired key at startup than after three
	// weeks of silent push failures
	if n, err := st.MismatchedKeyCount(context.Background(), env.vapidPublic); err == nil && n > 0 {
		logger.Warn("subscriptions bound to a different vapid key — each browser must re-subscribe (open the app once per device)",
			"count", n)
	}

	// the handlers are registered before the server starts, because an
	// unhandled SIGHUP terminates the process. A SIGTERM that arrives before
	// its handler is registered also terminates the process
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

// shutdownTimeout bounds http.Server.Shutdown (§10.1). It is well below the
// 10-second grace period of Docker for a stop.
const shutdownTimeout = 5 * time.Second

// serveUntil runs the server and the sweep until stop is canceled. The
// binary is PID 1 and gets no default signal handling (§10.1). When stop
// is canceled, serveUntil shuts down in the one safe order. First, the
// server stops taking requests. Next, serveUntil cancels the sweep's context
// and waits for the sweep. Only then does it close the database. As a
// result, the WAL checkpoint never runs during a tick that still writes
// delivery_error or deletes a subscription. The wait is short because every
// send in the sweep uses a child of the sweep's context.
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
			// a handler ran longer than shutdownTimeout: POST /api/push/test
			// can run 12 s (§7.5). Close ends the connections, and with them
			// the request context of that handler, so its sends stop. This is
			// an ordinary stop, not a failure to report.
			server.Close()
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
// The result of a reload shows only in the log. A `nag config check` before
// the HUP makes sure that a bad edit costs nothing.
func reloadOnHangup(hup <-chan os.Signal, live *config.Live, logger *slog.Logger) {
	for range hup {
		live.Reload(logger)
	}
}

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grexie/webshell/v2/pkg/config"
	"github.com/grexie/webshell/v2/pkg/logger"
	"github.com/grexie/webshell/v2/pkg/server"
)

func main() {
	log := logger.New()
	cfg := config.Load()
	app := server.New(cfg, log)

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-signals:
		log.Info("shutdown requested", "signal", sig.String())
	case err := <-errCh:
		if err != nil {
			log.Error("server failed", "error", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.Shutdown(ctx); err != nil {
		log.Error("shutdown failed", "error", err)
		os.Exit(1)
	}

	log.Info("shutdown complete", slog.String("addr", cfg.Addr()))
}

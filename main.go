package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"apimigrate/config"
	"apimigrate/proxy"
	"apimigrate/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration file")
	port := flag.Int("port", 0, "override the configured listen port (0 = use config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	log.Printf("starting api-migrate-quality-test run=%s", runID)
	log.Printf("mirroring %s ↔ %s (methods: %v)", cfg.Original, cfg.Migrated, cfg.IdempotentMethods)

	// Opening the store immediately flushes data from any previous run so
	// each run starts clean and the database never accumulates.
	db, err := store.New(cfg.DB.Path)
	if err != nil {
		log.Fatalf("db error: %v", err)
	}
	defer db.Close()
	log.Printf("sqlite database initialized (flushed) at %s", cfg.DB.Path)

	p, err := proxy.New(cfg, db, runID)
	if err != nil {
		log.Fatalf("proxy error: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", addr)
		log.Printf("report (markdown): http://%s%s", addr, cfg.Report.Endpoint)
		if cfg.Report.JSONEndpoint != "" {
			log.Printf("report (json):    http://%s%s", addr, cfg.Report.JSONEndpoint)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	log.Println("bye")
}
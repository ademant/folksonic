package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/tsirysndr/tinysonic/internal/cli"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/db"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/server"
)

func main() {
	args := cli.Parse()

	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	cli.PrintBanner(cfg.Host, cfg.Port, cfg.MusicDir)

	pool, err := db.Init(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	progress := &scanner.Progress{}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if !args.NoScan {
		go func() {
			log.Printf("starting library scan of %s", cfg.MusicDir)
			if _, err := scanner.Scan(pool, cfg.MusicDir, cfg.CoversDir, progress); err != nil {
				log.Printf("scan failed: %v", err)
			}
		}()
	}

	if !args.NoWatch {
		go func() {
			if err := scanner.Watch(ctx, pool, cfg.MusicDir, cfg.CoversDir, progress); err != nil {
				log.Printf("watch: %v", err)
			}
		}()
	}

	if err := server.Start(ctx, cfg, pool, progress); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}

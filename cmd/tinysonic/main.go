package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tsirysndr/tinysonic/internal/cli"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/db"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/server"
	"github.com/tsirysndr/tinysonic/internal/users"
)

func main() {
	// Account and pairing commands run directly against the database and exit.
	if len(os.Args) > 1 && (os.Args[1] == "user" || os.Args[1] == "pair") {
		if err := runAdmin(os.Args[1:]); err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(os.Stderr, "tinysonic: %v\n", err)
			}
			os.Exit(2)
		}
		return
	}

	args := cli.Parse()

	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	for _, w := range cfg.Warnings {
		log.Printf("warning: %s", w)
	}

	cli.PrintBanner(cfg.Host, cfg.Port, cfg.MusicDirs)

	pool, err := db.Init(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	created, err := users.Bootstrap(context.Background(), pool, cfg.Username, cfg.Password)
	if err != nil {
		log.Fatalf("users: %v", err)
	}
	if created {
		log.Printf("created admin account %q from the config file", cfg.Username)
	}
	if err := users.AdoptLegacyData(context.Background(), pool); err != nil {
		log.Fatalf("users: adopting existing playlists and stars: %v", err)
	}
	if n, _ := users.Count(context.Background(), pool); n == 0 {
		log.Printf("warning: no user accounts exist; create one with: tinysonic user add <name> --admin")
	}

	progress := &scanner.Progress{}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if !args.NoScan {
		go func() {
			log.Printf("starting library scan of %s", strings.Join(cfg.MusicDirs, ", "))
			if _, err := scanner.Scan(pool, cfg.MusicDirs, cfg.CoversDir, progress); err != nil {
				log.Printf("scan failed: %v", err)
			}
		}()
	}

	if !args.NoWatch {
		go func() {
			if err := scanner.Watch(ctx, pool, cfg.MusicDirs, cfg.CoversDir, progress); err != nil {
				log.Printf("watch: %v", err)
			}
		}()
	}

	if err := server.Start(ctx, cfg, pool, progress); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}

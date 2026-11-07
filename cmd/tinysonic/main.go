package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tsirysndr/tinysonic/internal/cli"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/db"
	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/server"
	"github.com/tsirysndr/tinysonic/internal/users"
)

func main() {
	// Account, invite, pairing and analysis commands run directly against the database and exit.
	if len(os.Args) > 1 && (os.Args[1] == "user" || os.Args[1] == "invite" || os.Args[1] == "pair" || os.Args[1] == "analysis") {
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
	level := cfg.LogLevel
	if args.LogLevel != "" {
		level = args.LogLevel
	}
	lvl, err := logging.ParseLevel(level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--log-level: %v\n", err)
		os.Exit(1)
	}
	logging.Setup(lvl)
	for _, w := range cfg.Warnings {
		logging.Warnf("%s", w)
	}

	cli.PrintBanner(cfg.Host, cfg.Port, cfg.MusicDirs)

	pool, err := db.Init(cfg.DatabasePath)
	if err != nil {
		logging.Fatalf("db: %v", err)
	}
	defer pool.Close()

	created, err := users.Bootstrap(context.Background(), pool, cfg.Username, cfg.Password)
	if err != nil {
		logging.Fatalf("users: %v", err)
	}
	if created {
		logging.Infof("created librarian account %q from the config file", cfg.Username)
	}
	if err := users.AdoptLegacyData(context.Background(), pool); err != nil {
		logging.Fatalf("users: adopting existing playlists and stars: %v", err)
	}
	if changed, err := db.SyncSongTags(context.Background(), pool, cfg.SongTags); err != nil {
		logging.Fatalf("db: song_tags: %v", err)
	} else if changed {
		logging.Infof("song_tags changed; every file will be re-read by the next scan")
	}
	if n, _ := users.Count(context.Background(), pool); n == 0 {
		logging.Warnf("no user accounts exist; create one with: tinysonic user add <name>")
	}

	progress := &scanner.Progress{}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if !args.NoScan {
		go func() {
			logging.Infof("starting library scan of %s", strings.Join(cfg.MusicDirs, ", "))
			if _, err := scanner.Scan(pool, cfg.MusicDirs, cfg.CoversDir, cfg.SongTags, progress); err != nil {
				logging.Errorf("scan failed: %v", err)
			}
		}()
	}

	if !args.NoWatch {
		go func() {
			if err := scanner.Watch(ctx, pool, cfg.MusicDirs, cfg.CoversDir, cfg.SongTags, progress); err != nil {
				logging.Errorf("watch: %v", err)
			}
		}()
	}

	if err := server.Start(ctx, cfg, pool, progress); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logging.Fatalf("server: %v", err)
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
	"github.com/tsirysndr/tinysonic/internal/worker"
)

// runWorkerCommand handles `tinysonic worker …` (#78):
//
//	worker key  [-c config] --user <name> --out <file>   on the server: a worker key in a file
//	worker pair [-c config] --user <name>                on the server: a one-time pairing token
//	worker run  [-c worker.toml] [--pair <token>]        on the desktop: analyse tracks
func runWorkerCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tinysonic worker run|pair|key …")
	}
	switch args[0] {
	case "run":
		return runWorker(args[1:])
	case "key":
		return workerKey(args[1:])
	case "pair":
		return workerPair(args[1:])
	}
	return fmt.Errorf("unknown worker command %q (run, pair, key)", args[0])
}

func runWorker(args []string) error {
	fs := flag.NewFlagSet("worker run", flag.ContinueOnError)
	cfgPath := fs.String("c", "worker.toml", "path to the worker's TOML file")
	pair := fs.String("pair", "", "pairing token from `tinysonic worker pair`; redeemed once and saved to the file")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	logging.Setup(slog.LevelInfo)
	if *pair != "" {
		if err := redeemInto(*cfgPath, *pair); err != nil {
			return err
		}
	}
	cfg, err := worker.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return worker.Run(ctx, cfg)
}

// redeemInto exchanges a pairing token for a worker key and saves it in the
// file. The file must not hold a key already.
func redeemInto(path, token string) error {
	cfg, err := worker.Read(path)
	if err != nil {
		return err
	}
	if cfg.Server == "" {
		return fmt.Errorf("%s: set server (the public address of the server) first", path)
	}
	if cfg.APIKey != "" {
		return fmt.Errorf("%s already holds a key; remove api_key to pair again", path)
	}
	key, err := worker.Redeem(context.Background(), cfg.Server, token, hostLabel())
	if err != nil {
		return fmt.Errorf("pairing: %w", err)
	}
	cfg.APIKey = key
	if err := worker.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "paired: the key is saved in %s\n", path)
	return nil
}

func hostLabel() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "worker"
}

func workerKey(args []string) error {
	fs := flag.NewFlagSet("worker key", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	name := fs.String("user", "", "account the worker key belongs to")
	out := fs.String("out", "", "file to write the worker configuration to (must not exist)")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if *name == "" || *out == "" {
		return usage(fs, "worker key [-c config] --user <username> --out <file>")
	}
	cfg, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.BaseURL == "" {
		return errors.New("set base_url in the config first: the worker needs the server's public address")
	}
	ctx := context.Background()
	u, err := users.ByName(ctx, pool, *name)
	if err != nil {
		return fmt.Errorf("user %q: %w", *name, err)
	}
	if u.Status != users.StatusActive {
		return fmt.Errorf("user %q is %s; only active users can own worker keys", u.Username, u.Status)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%s: %w", *out, err)
	}
	defer f.Close()
	key, err := users.CreateScopedAPIKey(ctx, pool, u.ID, "worker: "+*out, users.ScopeWorker, 0)
	if err != nil {
		os.Remove(*out)
		return err
	}
	if _, err := fmt.Fprint(f, workerTemplate(cfg.BaseURL, key)); err != nil {
		return err
	}
	fmt.Printf("worker key for %s written to %s (mode 0600)\n", u.Username, *out)
	fmt.Println("Copy it to the worker machine, set music_root and the folksentia command there,")
	fmt.Println("then delete this file. Revoke the key with `tinysonic user revoke-key` if it leaks.")
	return nil
}

func workerTemplate(server, key string) string {
	var b strings.Builder
	b.WriteString("# tinysonic analysis worker (#78). This file holds a secret: keep it private.\n")
	fmt.Fprintf(&b, "server = %q\n", server)
	fmt.Fprintf(&b, "api_key = %q\n", key)
	b.WriteString("# Where the server's music directory is mounted on this machine (absolute).\n")
	b.WriteString("music_root = \"\"\n")
	b.WriteString("batch = 5\n\n")
	b.WriteString("[folksentia]\n")
	b.WriteString("# Absolute path to the folksentia binary on this machine.\n")
	b.WriteString("command = \"\"\n")
	b.WriteString("timeout = \"120s\"\n")
	b.WriteString("# Optional memory cap for the tool, e.g. \"3G\".\n")
	b.WriteString("# memory_limit = \"3G\"\n")
	return b.String()
}

func workerPair(args []string) error {
	fs := flag.NewFlagSet("worker pair", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	name := fs.String("user", "", "account the worker key belongs to")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if *name == "" {
		return usage(fs, "worker pair [-c config] --user <username>")
	}
	cfg, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx := context.Background()
	u, err := users.ByName(ctx, pool, *name)
	if err != nil {
		return fmt.Errorf("user %q: %w", *name, err)
	}
	if u.Status != users.StatusActive {
		return fmt.Errorf("user %q is %s; only active users can own worker keys", u.Username, u.Status)
	}
	token, err := users.CreateScopedPairingToken(ctx, pool, u.ID, users.ScopeWorker)
	if err != nil {
		return err
	}
	if cfg.BaseURL == "" {
		fmt.Fprintln(os.Stderr, "warning: base_url is not set; put the server's public address in the worker file")
	}
	fmt.Printf("Pairing token (single use, valid for %d minutes):\n%s\n", int(users.PairingTTL.Minutes()), token)
	fmt.Println("On the worker machine, with a worker.toml that has server and music_root set:")
	fmt.Println("  tinysonic worker run -c worker.toml --pair <token>")
	return nil
}

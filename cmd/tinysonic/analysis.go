package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

// runAnalysis handles `tinysonic analysis recalc|pending`. It runs the
// assigned tools in this process, against the database directly; a running
// server sees the results on its next read.
func runAnalysis(args []string) error {
	const synopsis = "analysis recalc [-c config] [-features key,beats] (-all | SONG_ID…)\n       tinysonic analysis pending [-c config]"
	if len(args) == 0 || (args[0] != "recalc" && args[0] != "pending") {
		return errors.New("usage: tinysonic " + synopsis)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("analysis "+sub, flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	all := fs.Bool("all", false, "with recalc: every song")
	features := fs.String("features", "", "with recalc: comma-separated features (default: every assigned feature)")
	if err := fs.Parse(reorderFlags(rest)); err != nil {
		return err
	}
	ids := fs.Args()

	cfg, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	lvl, _ := logging.ParseLevel(cfg.LogLevel)
	logging.Setup(lvl)
	plan := analysis.NewPlan(cfg.Analysis, cfg.FFmpegPath)

	sel := analysis.Selection{Pending: true}
	if sub == "recalc" {
		if *all == (len(ids) > 0) {
			return usage(fs, synopsis)
		}
		sel = analysis.Selection{IDs: ids}
		if *all {
			sel.IDs = nil
		}
		if *features != "" {
			for f := range strings.SplitSeq(*features, ",") {
				f = strings.TrimSpace(f)
				if plan.Tool(f) == "" {
					return fmt.Errorf("no analysis tool is assigned to %q (assigned: %v)", f, plan.Assignments())
				}
				sel.Features = append(sel.Features, f)
			}
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tasks, err := plan.Select(ctx, pool, sel)
	if err != nil {
		return err
	}
	if sub == "recalc" && len(ids) > 0 && len(tasks) < len(ids) {
		fmt.Fprintf(os.Stderr, "warning: %d of %d songs not found or not selected for the features\n", len(ids)-len(tasks), len(ids))
	}
	cmds, err := plan.Commands(tasks)
	if err != nil {
		return err
	}
	var changed, failed int
	for i, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		out := plan.RunTask(ctx, pool, cmds, t)
		if out.Changed {
			changed++
		}
		if out.Failed {
			failed++
		}
		fmt.Fprintf(os.Stderr, "\r%d/%d", i+1, len(tasks))
	}
	if len(tasks) > 0 {
		fmt.Fprintln(os.Stderr)
	}
	fmt.Printf("%d tracks, %d changed, %d failed\n", len(tasks), changed, failed)
	return ctx.Err()
}

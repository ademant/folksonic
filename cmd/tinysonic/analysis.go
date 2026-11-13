package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

// runAnalysis handles `tinysonic analysis recalc|pending|export|import`.
// recalc and pending run the assigned tools in this process, against the
// database directly; a running server sees the results on its next read.
func runAnalysis(args []string) error {
	const synopsis = "analysis recalc [-c config] [-features key,beats] (-all | SONG_ID…)\n" +
		"       tinysonic analysis pending [-c config]\n" +
		"       tinysonic analysis export [-c config] [-features …] (-all | -pending | SONG_ID…) -out jobs.json\n" +
		"       tinysonic analysis import [-c config] [-dry-run] [-size-only] results.jsonl"
	if len(args) == 0 {
		return errors.New("usage: tinysonic " + synopsis)
	}
	switch args[0] {
	case "recalc", "pending":
	case "export":
		return runAnalysisExport(args[1:])
	case "import":
		return runAnalysisImport(args[1:])
	default:
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
				if t := plan.Tool(f); plan.Available()[t] == analysis.OfflineOnly {
					return fmt.Errorf("%s: %s is %s", f, t, analysis.OfflineOnly)
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
	if !sel.Pending {
		if err := plan.ResetAttempts(ctx, pool, tasks); err != nil {
			return err
		}
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

// runAnalysisExport writes the folksentia job list for an offline run (#105).
func runAnalysisExport(args []string) error {
	const synopsis = "analysis export [-c config] [-features bpm,beats,…] (-all | -pending | SONG_ID…) -out jobs.json"
	fs := flag.NewFlagSet("analysis export", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	all := fs.Bool("all", false, "every song")
	pending := fs.Bool("pending", false, "only songs with features folksentia has not analysed yet")
	features := fs.String("features", "", "comma-separated features (default: every feature assigned to folksentia)")
	out := fs.String("out", "", "file to write the job list to (\"-\" for stdout)")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	ids := fs.Args()
	modes := 0
	for _, b := range []bool{*all, *pending, len(ids) > 0} {
		if b {
			modes++
		}
	}
	if modes != 1 || *out == "" {
		return usage(fs, synopsis)
	}

	cfg, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	plan := analysis.NewPlan(cfg.Analysis, cfg.FFmpegPath)
	sel := analysis.Selection{Pending: *pending}
	if len(ids) > 0 {
		sel.IDs = ids
	}
	if *features != "" {
		for f := range strings.SplitSeq(*features, ",") {
			f = strings.TrimSpace(f)
			if plan.Tool(f) != analysis.OfflineTool {
				return fmt.Errorf("%q is not assigned to folksentia (assigned: %v)", f, plan.Assignments())
			}
			sel.Features = append(sel.Features, f)
		}
	}
	jobs, rep, err := plan.ExportJobs(context.Background(), pool, sel, cfg.MusicDirs, time.Now())
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *out == "-" {
		_, err = os.Stdout.Write(data)
	} else {
		err = writeNew(*out, data)
	}
	if err != nil {
		return err
	}
	if len(ids) > 0 && rep.Jobs < len(ids) {
		fmt.Fprintf(os.Stderr, "warning: %d of %d songs not found or not selected for the features\n", len(ids)-rep.Jobs, len(ids))
	}
	fmt.Fprintf(os.Stderr, "%d jobs written", rep.Jobs)
	if rep.Unreadable+rep.Outside > 0 {
		fmt.Fprintf(os.Stderr, "; left out: %d unreadable, %d outside the music folders", rep.Unreadable, rep.Outside)
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

// writeNew writes data to a new file, replacing it atomically if it exists.
func writeNew(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runAnalysisImport stores folksentia's results from an offline run (#105).
func runAnalysisImport(args []string) error {
	const synopsis = "analysis import [-c config] [-dry-run] [-size-only] results.jsonl"
	fs := flag.NewFlagSet("analysis import", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	dry := fs.Bool("dry-run", false, "check every line, store nothing")
	sizeOnly := fs.Bool("size-only", false, "accept results when the file size matches, whatever the modification time")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usage(fs, synopsis)
	}
	cfg, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	lvl, _ := logging.ParseLevel(cfg.LogLevel)
	logging.Setup(lvl)
	plan := analysis.NewPlan(cfg.Analysis, cfg.FFmpegPath)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rep, err := plan.ImportResults(ctx, pool, fs.Arg(0), analysis.ImportOptions{DryRun: *dry, SizeOnly: *sizeOnly})
	for _, p := range rep.Problems {
		fmt.Fprintln(os.Stderr, p)
	}
	verb := "stored"
	if *dry {
		verb = "would store"
	}
	fmt.Printf("%d lines: %s %d (%d changed, %d failed runs)", rep.Lines, verb, rep.Stored, rep.Changed, rep.Failed)
	if len(rep.Skipped) > 0 {
		reasons := slices.Sorted(maps.Keys(rep.Skipped))
		parts := make([]string, len(reasons))
		for i, r := range reasons {
			parts[i] = fmt.Sprintf("%d %s", rep.Skipped[r], r)
		}
		fmt.Printf("; skipped %s", strings.Join(parts, ", "))
	}
	fmt.Println()
	return err
}

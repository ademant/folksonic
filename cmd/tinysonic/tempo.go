package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/tsirysndr/tinysonic/internal/analysis"
)

// runAnalysisTempo prints the tempo report (#127): every song's stored tempo
// and how its BPM relates to the measured pulse, as calibration input for a
// player's playback-speed rule. It only reads.
func runAnalysisTempo(args []string) error {
	const synopsis = "analysis tempo [-c config] [-tsv] [-summary]"
	fs := flag.NewFlagSet("analysis tempo", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	tsv := fs.Bool("tsv", false, "tab-separated lines with a header, the summary on stderr")
	summaryOnly := fs.Bool("summary", false, "only the summary")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usage(fs, synopsis)
	}
	_, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rows, err := analysis.TempoReport(ctx, pool)
	if err != nil {
		return err
	}
	summaryOut := io.Writer(os.Stdout)
	if *tsv {
		summaryOut = os.Stderr
	}
	if !*summaryOnly {
		if err := writeTempoRows(os.Stdout, rows, *tsv); err != nil {
			return err
		}
		if !*tsv {
			fmt.Println()
		}
	}
	return writeTempoSummary(summaryOut, rows)
}

// tempoColumns are the report's columns: the song, its BPM and source, the
// tinysonicTempo keys, then the values only the report shows.
func tempoColumns() []string {
	cols := []string{"id", "bpm", "source"}
	for _, k := range analysis.TempoKeys {
		cols = append(cols, k.Name)
	}
	return append(cols, "pulsePerMin", "barScore", "ratio", "artist", "title")
}

func tempoCells(r analysis.TempoRow) []string {
	cells := []string{r.ID, nullFloat(r.BPM, 1), nullString(r.Source)}
	for _, k := range analysis.TempoKeys {
		cells = append(cells, tempoCell(k.Value(r.Tempo)))
	}
	pulse := "-"
	if p := r.Tempo.PulsePerMin(); p > 0 {
		pulse = strconv.FormatFloat(p, 'f', 1, 64)
	}
	score := "-"
	if r.Tempo.Rhythm != nil && r.Tempo.Rhythm.BarScore != nil {
		score = strconv.FormatFloat(*r.Tempo.Rhythm.BarScore, 'f', 2, 64)
	}
	return append(cells, pulse, score, r.Ratio, oneLine(r.Artist), oneLine(r.Title))
}

func tempoCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case float64:
		return strconv.FormatFloat(x, 'f', 2, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprint(x)
	}
}

func nullFloat(v sql.NullFloat64, prec int) string {
	if !v.Valid {
		return "-"
	}
	return strconv.FormatFloat(v.Float64, 'f', prec, 64)
}

func nullString(v sql.NullString) string {
	if !v.Valid || v.String == "" {
		return "-"
	}
	return v.String
}

// oneLine keeps tags with tabs or newlines from breaking the columns.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// writeTempoRows writes one line per song, aligned or tab-separated.
func writeTempoRows(w io.Writer, rows []analysis.TempoRow, tsv bool) error {
	out := w
	var tw *tabwriter.Writer
	if !tsv {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		out = tw
	}
	if _, err := fmt.Fprintln(out, strings.Join(tempoColumns(), "\t")); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintln(out, strings.Join(tempoCells(r), "\t")); err != nil {
			return err
		}
	}
	if tw != nil {
		return tw.Flush()
	}
	return nil
}

// writeTempoSummary counts the songs per ratio of BPM to pulse rate, for
// each BPM source and in total.
func writeTempoSummary(w io.Writer, rows []analysis.TempoRow) error {
	counts := map[string]map[string]int{} // source → ratio → songs
	var withBPM, withPulse, both int
	for _, r := range rows {
		hasBPM, hasPulse := r.BPM.Valid, r.Tempo.PulsePerMin() > 0
		if hasBPM {
			withBPM++
		}
		if hasPulse {
			withPulse++
		}
		if !hasBPM || !hasPulse {
			continue
		}
		both++
		src := nullString(r.Source)
		if counts[src] == nil {
			counts[src] = map[string]int{}
		}
		counts[src][r.Ratio]++
	}
	if _, err := fmt.Fprintf(w, "%d songs: %d with bpm, %d with a measured pulse, %d with both\n",
		len(rows), withBPM, withPulse, both); err != nil {
		return err
	}
	if both == 0 {
		return nil
	}
	sources := slices.Sorted(maps.Keys(counts))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "bpm/pulse\t"+strings.Join(sources, "\t")+"\tall\t")
	for _, class := range analysis.TempoRatioClasses {
		if class == analysis.RatioUnknown {
			continue
		}
		line := []string{class}
		total := 0
		for _, src := range sources {
			n := counts[src][class]
			total += n
			line = append(line, strconv.Itoa(n))
		}
		line = append(line, fmt.Sprintf("%d (%.0f%%)", total, 100*float64(total)/float64(both)))
		fmt.Fprintln(tw, strings.Join(line, "\t")+"\t")
	}
	return tw.Flush()
}

package cli

import (
	"flag"
	"fmt"
)

const Version = "0.1.0"

const Banner = `
 _   _                             _
| |_(_)_ __  _   _ ___  ___  _ __ (_) ___
| __| | '_ \| | | / __|/ _ \| '_ \| |/ __|
| |_| | | | | |_| \__ \ (_) | | | | | (__
 \__|_|_| |_|\__, |___/\___/|_| |_|_|\___|
             |___/    a tiny Subsonic server in Go
`

type Args struct {
	ConfigPath string
	NoScan     bool
	NoWatch    bool
}

func Parse() Args {
	var a Args
	flag.StringVar(&a.ConfigPath, "config", "smolsonic.toml", "Path to the TOML config file")
	flag.StringVar(&a.ConfigPath, "c", "smolsonic.toml", "Path to the TOML config file (shorthand)")
	flag.BoolVar(&a.NoScan, "no-scan", false, "Skip the startup library scan")
	flag.BoolVar(&a.NoWatch, "no-watch", false, "Disable the filesystem watcher that keeps the library in sync")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"tinysonic %s — a tiny Subsonic-compatible music server\n\n"+
				"Usage: tinysonic [OPTIONS]\n\n"+
				"Options:\n", Version)
		flag.PrintDefaults()
	}
	flag.Parse()
	return a
}

func PrintBanner(host string, port int, musicDirs []string) {
	const (
		hotPink        = "\x1b[1;38;2;255;42;109m"
		electricCyan   = "\x1b[1;38;2;5;217;232m"
		neonMagenta    = "\x1b[1;38;2;255;113;206m"
		electricPurple = "\x1b[1;38;2;185;103;255m"
		dim            = "\x1b[2m"
		reset          = "\x1b[0m"
	)
	fmt.Print(hotPink + Banner + reset)
	fmt.Printf("  %slistening%s %s→%s %shttp://%s:%d%s\n", electricCyan, reset, dim, reset, neonMagenta, host, port, reset)
	for i, d := range musicDirs {
		label := "library "
		if i > 0 {
			label = "        "
		}
		fmt.Printf("  %s%s%s %s→%s %s%s%s\n", electricCyan, label, reset, dim, reset, electricPurple, d, reset)
	}
	fmt.Println()
}

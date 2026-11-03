package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/term"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/db"
	"github.com/tsirysndr/tinysonic/internal/server"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// runAdmin handles `tinysonic user …`, `tinysonic invite …` and
// `tinysonic pair …`. These run against
// the database directly, so they need access to the server's data directory,
// not to the running HTTP server.
func runAdmin(args []string) error {
	switch args[0] {
	case "user":
		return runUser(args[1:])
	case "invite":
		return runInvite(args[1:])
	case "pair":
		return runPair(args[1:])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func openDB(cfgPath string) (*config.Config, *sql.DB, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	pool, err := db.Init(cfg.DatabasePath)
	if err != nil {
		return nil, nil, err
	}
	// Same first-admin bootstrap as server start, so `user add` works on a
	// fresh install before the server has ever run.
	if _, err := users.Bootstrap(context.Background(), pool, cfg.Username, cfg.Password); err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err := users.AdoptLegacyData(context.Background(), pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return cfg, pool, nil
}

func usage(fs *flag.FlagSet, synopsis string) error {
	fmt.Fprintf(fs.Output(), "usage: tinysonic %s\n", synopsis)
	fs.PrintDefaults()
	return flag.ErrHelp
}

func runUser(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tinysonic user add|list|approve|disable|enable|remove|set-password")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	admin := fs.Bool("admin", false, "with add: create an administrator")
	if err := fs.Parse(reorderFlags(rest)); err != nil {
		return err
	}
	names := fs.Args()

	_, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx := context.Background()

	switch sub {
	case "add":
		if len(names) != 1 {
			return usage(fs, "user add [-c config] [--admin] <username>")
		}
		pw, err := readPassword("Password: ", true)
		if err != nil {
			return err
		}
		role := users.RoleUser
		if *admin {
			role = users.RoleAdmin
		}
		u, err := users.Create(ctx, pool, names[0], pw, role, users.StatusActive)
		if err != nil {
			return err
		}
		fmt.Printf("created %s (%s, active)\n", u.Username, u.Role)

	case "list":
		all, err := users.List(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Printf("%-20s %-8s %s\n", "USERNAME", "ROLE", "STATUS")
		for _, u := range all {
			fmt.Printf("%-20s %-8s %s\n", u.Username, u.Role, u.Status)
		}

	case "approve", "disable", "enable", "remove", "set-password":
		if len(names) != 1 {
			return usage(fs, "user "+sub+" [-c config] <username>")
		}
		u, err := users.ByName(ctx, pool, names[0])
		if err != nil {
			return fmt.Errorf("user %q: %w", names[0], err)
		}
		switch sub {
		case "approve":
			err = users.SetStatus(ctx, pool, u.ID, users.StatusActive, 0)
		case "disable":
			err = users.SetStatus(ctx, pool, u.ID, users.StatusDisabled, 0)
		case "enable":
			err = users.SetStatus(ctx, pool, u.ID, users.StatusActive, 0)
		case "remove":
			err = users.Delete(ctx, pool, u.ID)
		case "set-password":
			pw, perr := readPassword("New password: ", true)
			if perr != nil {
				return perr
			}
			err = users.SetPassword(ctx, pool, u.ID, pw)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s done\n", u.Username, sub)

	default:
		return fmt.Errorf("unknown user command %q", sub)
	}
	return nil
}

func runInvite(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tinysonic invite create|list|revoke")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("invite "+sub, flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	admin := fs.Bool("admin", false, "with create: the invitee becomes an administrator")
	expires := fs.String("expires", "7d", "with create: validity, in days (7d) or a Go duration (12h)")
	note := fs.String("note", "", "with create: who the invite is for, shown by list")
	boundName := fs.String("username", "", "with create: only this username may redeem the code; the account is passkey-only")
	if err := fs.Parse(reorderFlags(rest)); err != nil {
		return err
	}
	pos := fs.Args()

	cfg, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx := context.Background()

	switch sub {
	case "create":
		if len(pos) != 0 {
			return usage(fs, "invite create [-c config] [--expires 7d] [--admin] [--note text] [--username name]")
		}
		ttl, err := parseExpiry(*expires)
		if err != nil {
			return err
		}
		role := users.RoleUser
		if *admin {
			role = users.RoleAdmin
		}
		code, inv, err := users.CreateInvite(ctx, pool, role, *boundName, *note, ttl)
		if err != nil {
			return err
		}
		fmt.Printf("invite %d (%s), valid until %s, single use\n", inv.ID, inv.Role, inv.ExpiresAt)
		if inv.Username.Valid {
			fmt.Printf("bound to username %q; the account is created when its passkey is registered\n", inv.Username.String)
		}
		fmt.Println(code)
		if cfg.BaseURL != "" {
			fmt.Println(strings.TrimRight(cfg.BaseURL, "/") + "/login?invite=" + code)
		} else {
			fmt.Fprintln(os.Stderr, "note: set base_url in the config to print a registration link")
		}
		fmt.Fprintln(os.Stderr, "The code is shown only now.")

	case "list":
		all, err := users.ListInvites(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Printf("%-4s %-6s %-8s %-21s %-12s %s\n", "ID", "ROLE", "STATE", "EXPIRES", "USED BY", "NOTE")
		for _, i := range all {
			fmt.Printf("%-4d %-6s %-8s %-21s %-12s %s\n", i.ID, i.Role, i.State(), i.ExpiresAt, i.UsedBy.String, i.Note)
		}

	case "revoke":
		if len(pos) != 1 {
			return usage(fs, "invite revoke [-c config] <id>")
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return fmt.Errorf("invite id %q: not a number", pos[0])
		}
		if err := users.RevokeInvite(ctx, pool, id); err != nil {
			if errors.Is(err, users.ErrNotFound) {
				return fmt.Errorf("invite %d: no such unused invite", id)
			}
			return err
		}
		fmt.Printf("invite %d revoked\n", id)

	default:
		return fmt.Errorf("unknown invite command %q", sub)
	}
	return nil
}

// parseExpiry accepts a number of days ("7d") or a Go duration ("12h").
func parseExpiry(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("expires %q: want days like 7d or a duration like 12h", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("expires %q: want days like 7d or a duration like 12h", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("expires %q: must be positive", s)
	}
	return d, nil
}

func runPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	name := fs.String("user", "", "username to pair a device for")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if *name == "" {
		return usage(fs, "pair [-c config] --user <username>")
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
		return fmt.Errorf("user %q is %s; only active users can pair devices", u.Username, u.Status)
	}
	token, err := users.CreatePairingToken(ctx, pool, u.ID)
	if err != nil {
		return err
	}

	base := cfg.BaseURL
	if base == "" {
		host := cfg.Host
		if host == "0.0.0.0" || host == "" {
			host = "localhost"
		}
		base = fmt.Sprintf("http://%s:%d", host, cfg.Port)
		fmt.Fprintln(os.Stderr, "warning: base_url is not set; the QR code uses", base,
			"which the phone may not reach. Set base_url in the config.")
	}
	uri := server.PairURI(base, token)
	code, err := qrcode.New(uri, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Println(code.ToSmallString(false))
	fmt.Printf("Scan with the tinysonic app. Valid for %d minutes, single use.\n", int(users.PairingTTL.Minutes()))
	fmt.Println(uri)
	return nil
}

// readPassword reads a password from the terminal without echo. When stdin is
// not a terminal (scripts), it reads one line from stdin instead.
func readPassword(prompt string, confirm bool) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		pw := strings.TrimRight(string(line), "\r\n")
		if pw == "" {
			return "", errors.New("empty password")
		}
		return pw, nil
	}
	fmt.Fprint(os.Stderr, prompt)
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if len(a) == 0 {
		return "", errors.New("empty password")
	}
	if confirm {
		fmt.Fprint(os.Stderr, "Repeat: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
	}
	return string(a), nil
}

// reorderFlags moves flags that appear after positional arguments in front of
// them, so `tinysonic user add alice -c cfg.toml` parses like the flags came
// first. Go's flag package stops at the first positional argument otherwise.
func reorderFlags(args []string) []string {
	valueFlags := map[string]bool{"c": true, "user": true, "expires": true, "note": true, "username": true}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if valueFlags[strings.TrimLeft(a, "-")] && i+1 < len(args) && !strings.Contains(a, "=") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

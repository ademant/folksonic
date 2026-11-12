package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// runDevice lists and removes an account's paired apps (#101). Removing a
// device makes its next request fail; its key is only a public key, so
// nothing secret needs revoking.
func runDevice(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tinysonic device list|remove -c config --user <username> [id]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("device "+sub, flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	name := fs.String("user", "", "username whose devices to manage")
	if err := fs.Parse(reorderFlags(rest)); err != nil {
		return err
	}
	if *name == "" {
		return usage(fs, "device "+sub+" -c config --user <username>")
	}
	_, pool, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx := context.Background()
	u, err := users.ByName(ctx, pool, *name)
	if err != nil {
		return fmt.Errorf("user %q: %w", *name, err)
	}

	switch sub {
	case "list":
		list, err := users.Devices(ctx, pool, u.ID)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no paired devices")
		}
		for _, d := range list {
			last := d.LastUsedAt
			if last == "" {
				last = "never"
			}
			fmt.Printf("%d\t%s\tpaired %s\tlast used %s\n", d.ID, d.Name, d.CreatedAt, last)
		}
		return nil
	case "remove":
		if len(fs.Args()) != 1 {
			return usage(fs, "device remove -c config --user <username> <id>")
		}
		id, err := strconv.ParseInt(fs.Args()[0], 10, 64)
		if err != nil {
			return fmt.Errorf("device id %q: %w", fs.Args()[0], err)
		}
		if err := users.RemoveDevice(ctx, pool, u.ID, id); err != nil {
			if errors.Is(err, users.ErrNotFound) {
				return fmt.Errorf("no device %d for %s", id, *name)
			}
			return err
		}
		fmt.Printf("removed device %d from %s\n", id, *name)
		return nil
	}
	return fmt.Errorf("unknown device command %q", sub)
}

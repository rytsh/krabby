package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/transfer"
)

func runTransfer(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) < 2 || args[0] != "sync" {
		return fmt.Errorf("usage: krabby sync export --output FILE [--since STATUS.json] | sync import --input FILE | sync status")
	}
	fs := flag.NewFlagSet("krabby sync "+args[1], flag.ContinueOnError)
	output := fs.String("output", "", "export archive path (outside data_dir)")
	input := fs.String("input", "", "archive to import")
	since := fs.String("since", "", "target status JSON; omit for a full snapshot")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	// Match serving-process sizing; offline bw restore also needs bounded caches.
	budget := memlimit.NewWithOverrides(cfg.Memory.LimitBytes, cfg.Memory.Ratio, memlimit.Overrides{VectorCache: cfg.Memory.VectorCacheBytes})
	budget.Apply()
	memlimit.Set(budget)
	// The active pointer is atomic; status is safe while the server is running.
	if args[1] == "status" {
		if *input != "" || *output != "" || *since != "" {
			return fmt.Errorf("status does not accept file flags")
		}
		active, err := transfer.Active(cfg.DataDir)
		if err != nil {
			return err
		}
		if active == nil {
			return fmt.Errorf("no imported dataset")
		}
		return json.NewEncoder(os.Stdout).Encode(active.Manifest.Status)
	}
	unlock, err := transfer.Lock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	var status transfer.Status
	switch args[1] {
	case "export":
		if cfg.ReadOnly {
			return fmt.Errorf("cannot export in read_only mode")
		}
		if *output == "" || *input != "" {
			return fmt.Errorf("export requires --output FILE and optionally --since STATUS.json")
		}
		var cursor *transfer.Status
		if *since != "" {
			s, err := transfer.ReadStatus(*since)
			if err != nil {
				return err
			}
			cursor = &s
		}
		status, err = transfer.Export(ctx, cfg.DataDir, *output, config.Version, cursor)
	case "import":
		if !cfg.ReadOnly {
			return fmt.Errorf("import requires read_only: true")
		}
		if *input == "" || *output != "" || *since != "" {
			return fmt.Errorf("import requires --input FILE")
		}
		status, err = transfer.Import(ctx, cfg.DataDir, *input, config.Version)
	default:
		return fmt.Errorf("unknown sync command %q", args[1])
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}

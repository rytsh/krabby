// Package storage opens the embedded state database.
package storage

import (
	"fmt"
	"os"

	"github.com/rakunlabs/bw"
)

// Open creates/opens the bw database at path.
func Open(path string) (*bw.DB, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir; %w", err)
	}

	db, err := bw.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open state db %s; %w", path, err)
	}

	return db, nil
}

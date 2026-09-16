package transfer

import (
	"fmt"
	"path/filepath"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/taskstore"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/storage"
)

// Validate the same bucket registrations the reader will need before changing
// the active pointer. Physical read-only opens turn a missing bucket or schema
// migration into an import error, including across development builds that both
// report v0.0.0. Nothing may migrate the imported version line.
func validateReader(dir string, m Manifest) error {
	db, err := storage.OpenReadOnly(filepath.Join(dir, "state"))
	if err != nil {
		return err
	}
	defer db.Close()
	checks := []func() error{
		func() error { _, err := registry.New(db); return err },
		func() error { _, err := coderag.NewTextStore(db); return err },
		func() error { _, err := rag.NewTextStore(db); return err },
		func() error { _, err := taskstore.New(db); return err },
		func() error { _, err := credentials.New(db, filepath.Join(dir, "keys")); return err },
		func() error { _, err := settings.New(db, settings.Defaults()); return err },
		func() error { _, err := websource.New(db); return err },
		func() error { _, err := apicatalog.New(db); return err },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return fmt.Errorf("reader schema is incompatible with publisher: %w", err)
		}
	}
	for _, name := range databases[1:] {
		if _, exists := m.Databases[name]; !exists {
			continue
		}
		s, err := vectorstore.NewReadOnly(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("reader vector schema: %w", err)
		}
		if err := s.Close(); err != nil {
			return err
		}
	}
	return nil
}

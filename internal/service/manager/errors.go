package manager

import (
	"errors"
	"fmt"
)

// ErrNotFound marks a lookup of something krabby does not have: an untracked
// repository, an unknown web source or API service, a page, an operation, or
// an artefact not built yet. Transports map it to "not found" rather than
// guessing from the message.
var ErrNotFound = errors.New("not found")

// notFoundError keeps the message exactly as written; errors.Is still sees
// ErrNotFound.
type notFoundError struct{ msg string }

func (e notFoundError) Error() string { return e.msg }

func (notFoundError) Is(target error) bool { return target == ErrNotFound }

func notFound(format string, args ...any) error {
	return notFoundError{msg: fmt.Sprintf(format, args...)}
}

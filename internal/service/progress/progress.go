// Package progress carries an optional progress reporter through a context.
//
// It exists for krabby's plugin boundaries — the web-source Fetcher and the
// docs Generator — where reporting is optional, ambient and scoped to exactly
// one call. Passing it in the context keeps those interfaces about the work
// they describe: an implementation that cannot say how much is left (or does
// not care to) needs no signature, and adding a reporter never breaks one.
//
// Concrete in-repo services (rag, coderag) take an explicit callback instead:
// there is no interface to protect there, and an explicit parameter is easier
// to follow.
package progress

import "context"

// Func reports how far a step has got: done items out of total expected. A
// total of 0 means the total is not known (yet).
//
// It may be called from several goroutines, so an implementation must be safe
// for concurrent use.
type Func func(done, total int)

type key struct{}

// With returns a context carrying fn. A nil fn returns ctx unchanged.
func With(ctx context.Context, fn Func) context.Context {
	if fn == nil {
		return ctx
	}

	return context.WithValue(ctx, key{}, fn)
}

// Report publishes progress if ctx carries a reporter, and does nothing
// otherwise, so callers can report unconditionally.
func Report(ctx context.Context, done, total int) {
	if ctx == nil {
		return
	}
	if fn, ok := ctx.Value(key{}).(Func); ok && fn != nil {
		fn(done, total)
	}
}

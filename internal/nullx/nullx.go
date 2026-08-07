// Package nullx implements krabby's partial-update rule on top of
// types.Null[T]: absent = keep, null = clear, value = override.
//
// It lives in its own package because every partial-update surface has to agree
// on what an omitted field means — provider configs, the web-source envelope,
// the API-catalog envelope and settings all merge the same way. Keeping the
// rule in one place is what stops the halves of a single request from
// disagreeing: a config that merges per field wrapped in an envelope that is
// replaced wholesale silently wipes whatever the caller did not restate.
package nullx

import "github.com/worldline-go/types"

// Merge returns the update value when the field was present in the update JSON
// (set to a value OR explicitly null), otherwise the stored value.
//
// It rests on types.Null's ParsedNull marker, which is the one thing a plain Go
// zero value cannot express: whether the client said "leave this alone" or
// "make this empty".
func Merge[T any](update, stored types.Null[T]) types.Null[T] {
	if update.Valid || update.ParsedNull {
		return update
	}

	return stored
}

// Present reports whether the field was in the update JSON at all, with either
// a value or an explicit null. Use it when a merge is not a straight
// replacement — a secret that is kept unless explicitly cleared, say.
func Present[T any](n types.Null[T]) bool {
	return n.Valid || n.ParsedNull
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/observability/metrics"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
)

// Request body ceilings. defaultBodyLimit covers every ordinary JSON request
// (settings, a source config, a repo's overrides); pushBodyLimit is for the
// push and import endpoints, which carry whole documents and are bounded
// again by the manager.
const (
	defaultBodyLimit = 1 << 20
	pushBodyLimit    = 48 << 20
)

// errBodyNotJSON is returned for a body sent with a content type other than
// JSON.
var errBodyNotJSON = errors.New("request body must be JSON (Content-Type: application/json)")

// decodeJSON decodes a JSON request body into dst.
//
// It refuses what ada's Bind let through silently. A body with another
// content type (curl -d sends form encoding by default) was not decoded at
// all and left dst at its zero value, so a settings save did nothing, a
// repo's overrides were wiped and a concurrency limit reset to its default,
// each answered 200. The body is also capped: Bind read it whole, whatever
// its size.
//
// A missing Content-Type is accepted as JSON; a client that sends none means
// the body it sent. Unknown fields are allowed, so a client written for a
// newer krabby still works against this one.
func decodeJSON(c *ada.Context, dst any, limit int64) error {
	if ct := c.Request.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || (mediaType != "application/json" && mediaType != "text/json" && !isJSONSuffix(mediaType)) {
			return errBodyNotJSON
		}
	}
	if c.Request.Body == nil {
		return errors.New("request body is empty")
	}

	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, c.Request.Body, limit))
	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			return fmt.Errorf("request body is larger than %d bytes", limit)
		case errors.Is(err, io.EOF):
			return errors.New("request body is empty")
		default:
			return fmt.Errorf("invalid JSON body; %w", err)
		}
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("request body holds more than one JSON value")
	}

	return nil
}

// isJSONSuffix reports a structured-syntax JSON type such as
// application/merge-patch+json.
func isJSONSuffix(mediaType string) bool {
	return len(mediaType) > 5 && mediaType[len(mediaType)-5:] == "+json"
}

// bindJSON decodes a required JSON body with the default limit. The error is
// the caller's to answer with 400.
func bindJSON(c *ada.Context, dst any) error {
	return bindJSONLimit(c, dst, defaultBodyLimit)
}

func bindJSONLimit(c *ada.Context, dst any, limit int64) error {
	if err := decodeJSON(c, dst, limit); err != nil {
		return badRequest(err)
	}

	return nil
}

// bindOptionalJSON decodes a request body the caller may legitimately omit. An
// absent or empty body leaves dst at its zero value instead of failing, so an
// endpoint can gain optional parameters without breaking clients that post
// nothing.
func bindOptionalJSON(c *ada.Context, dst any) error {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}

	err := decodeJSON(c, dst, defaultBodyLimit)
	if err != nil && err.Error() == "request body is empty" {
		return nil
	}
	if err != nil {
		return badRequest(err)
	}

	return nil
}

// requestError is a request the caller got wrong; handlers answer it with 400.
type requestError struct{ error }

func (e requestError) Unwrap() error { return e.error }

func badRequest(err error) error { return requestError{err} }

// sendError answers err with the status its kind calls for, and fallback
// when it is of no kind this knows.
//
// Handlers used to pick one status for every failure: 404 for any file or
// doc error, 400 for any search error. An embedder outage then read as a bad
// query and a disabled feature as a missing file, and a client could not tell
// what to fix. Known kinds are mapped here; fallback keeps each handler's
// existing answer for the rest, which is mostly input validation.
func sendError(c *ada.Context, err error, fallback int) error {
	status := fallback
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, manager.ErrDocsDisabled), errors.Is(err, manager.ErrCodeRAGDisabled),
		errors.Is(err, manager.ErrNoWebSources), errors.Is(err, manager.ErrNoAPICatalog),
		errors.Is(err, manager.ErrManagerClosed):
		status = http.StatusServiceUnavailable
	case errors.Is(err, registry.ErrAmbiguousRepo):
		status = http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case metrics.Outcome(err) == metrics.OutcomeInvalid, isRequestError(err):
		status = http.StatusBadRequest
	}

	return c.SetStatus(status).SendJSON(map[string]string{"error": err.Error()})
}

func isRequestError(err error) bool {
	var re requestError

	return errors.As(err, &re)
}

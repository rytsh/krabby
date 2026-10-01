package server

import (
	"errors"
	"io"

	"github.com/rakunlabs/ada"
)

// bindOptionalJSON decodes a request body the caller may legitimately omit. An
// absent or empty body leaves dst at its zero value instead of failing, so an
// endpoint can gain optional parameters without breaking clients that post
// nothing.
func bindOptionalJSON(c *ada.Context, dst any) error {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}

	if err := c.Bind(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}

		return err
	}

	return nil
}

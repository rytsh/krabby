package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/rakunlabs/ada"
	"github.com/rytsh/krabby/internal/service/bigpicture"
)

func pictureError(c *ada.Context, err error) error {
	status, message := http.StatusInternalServerError, "big picture operation failed"
	switch {
	case errors.Is(err, bigpicture.ErrGenerationUnavailable):
		status, message = http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, bigpicture.ErrInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, bigpicture.ErrNotFound):
		status, message = http.StatusNotFound, err.Error()
	case errors.Is(err, bigpicture.ErrConflict), errors.Is(err, bigpicture.ErrExists):
		status, message = http.StatusConflict, err.Error()
	}
	return c.SetStatus(status).SendJSON(map[string]string{"error": message})
}

func decodePicture(c *ada.Context, dst any, limit int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, c.Request.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return bigpicture.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return bigpicture.ErrInvalid
	}
	return nil
}

func generateBigPicture(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := s.TriggerBigPictureGeneration(c.Request.Context(), c.Request.PathValue("name")); err != nil {
			return pictureError(c, err)
		}
		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"status": "queued", "scope": "bigpicture:" + c.Request.PathValue("name")})
	}
}

func listBigPictures(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query()
		page, err := s.ListBigPictures(c.Request.Context(), bigpicture.ListOptions{Namespace: q.Get("namespace"), Query: q.Get("q"), Page: queryInt(q.Get("page"), 1), PerPage: queryInt(q.Get("per_page"), 20)})
		if err != nil {
			return pictureError(c, err)
		}
		return c.SendJSON(page)
	}
}

func getBigPicture(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		p, err := s.BigPicture(c.Request.Context(), c.Request.PathValue("name"))
		if err != nil {
			return pictureError(c, err)
		}
		return c.SendJSON(p)
	}
}

func saveBigPicture(s bigPictureService, create bool) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var cfg bigpicture.Config
		if err := decodePicture(c, &cfg, 96<<10); err != nil {
			return pictureError(c, err)
		}
		name := ""
		if !create {
			name = c.Request.PathValue("name")
			if cfg.Name == "" {
				cfg.Name = name
			}
		}
		p, err := s.SaveBigPicture(c.Request.Context(), name, cfg)
		if err != nil {
			return pictureError(c, err)
		}
		if create {
			c.SetStatus(http.StatusCreated)
		}
		return c.SendJSON(p)
	}
}

func deleteBigPicture(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		version, err := strconv.ParseUint(c.Request.URL.Query().Get("expected_version"), 10, 64)
		if err != nil || version == 0 {
			return pictureError(c, bigpicture.ErrInvalid)
		}
		if err := s.DeleteBigPicture(c.Request.Context(), c.Request.PathValue("name"), version); err != nil {
			return pictureError(c, err)
		}
		return c.SendJSON(map[string]bool{"ok": true})
	}
}

func publishBigPicture(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var pub bigpicture.Publication
		if err := decodePicture(c, &pub, 8<<20); err != nil {
			return pictureError(c, err)
		}
		snapshot, err := s.PublishBigPicture(c.Request.Context(), c.Request.PathValue("name"), pub)
		if err != nil {
			return pictureError(c, err)
		}
		return c.SetStatus(http.StatusCreated).SendJSON(snapshot)
	}
}

func bigPictureSnapshot(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		snapshot, err := s.BigPictureSnapshot(c.Request.Context(), c.Request.PathValue("name"), c.Request.URL.Query().Get("revision"))
		if err != nil {
			return pictureError(c, err)
		}
		return c.SendJSON(snapshot)
	}
}

func readBigPictureDocument(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query()
		offset := int64(0)
		if q.Get("offset") != "" {
			value, err := strconv.ParseInt(q.Get("offset"), 10, 64)
			if err != nil {
				return pictureError(c, bigpicture.ErrInvalid)
			}
			offset = value
		}
		doc, err := s.ReadBigPictureDocument(c.Request.Context(), c.Request.PathValue("name"), q.Get("revision"), q.Get("path"), offset, queryInt(q.Get("max_bytes"), 32768))
		if err != nil {
			return pictureError(c, err)
		}
		return c.SendJSON(doc)
	}
}

func bigPictureSourceOptions(s bigPictureService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query()
		page, err := s.BigPictureSourceOptions(c.Request.Context(), q.Get("kind"), q.Get("q"), queryInt(q.Get("page"), 1), queryInt(q.Get("per_page"), 20))
		if err != nil {
			return pictureError(c, err)
		}
		return c.SendJSON(page)
	}
}

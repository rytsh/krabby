package browserextension

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"unicode"
)

const archiveRoot = "krabby-browser-extension"

//go:embed manifest.json bridge.js service-worker.js popup.html popup.js popup.css README.md icons/*.png
var files embed.FS

// Archive returns an unpacked-extension ZIP whose manifest identifies the
// running Krabby build. Chrome requires a numeric version, while version_name
// preserves the exact release string shown by Krabby (including prereleases).
func Archive(version string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		content, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		if name == "manifest.json" {
			content, err = versionedManifest(content, version)
			if err != nil {
				return err
			}
		}

		w, err := zw.Create(archiveRoot + "/" + name)
		if err != nil {
			return err
		}
		_, err = w.Write(content)

		return err
	})
	if err != nil {
		_ = zw.Close()

		return nil, fmt.Errorf("build browser extension archive; %w", err)
	}

	w, err := zw.Create(archiveRoot + "/KRABBY_VERSION.txt")
	if err != nil {
		_ = zw.Close()

		return nil, err
	}
	if _, err := w.Write([]byte(strings.TrimSpace(version) + "\n")); err != nil {
		_ = zw.Close()

		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func versionedManifest(content []byte, version string) ([]byte, error) {
	var manifest map[string]any
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("decode extension manifest; %w", err)
	}

	versionName := strings.TrimSpace(version)
	if versionName == "" {
		versionName = "v0.0.0"
	}
	manifest["version"] = chromeVersion(versionName)
	manifest["version_name"] = versionName

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode extension manifest; %w", err)
	}

	return append(out, '\n'), nil
}

// chromeVersion reduces release strings such as v1.4.0-3-gabc to Chrome's
// one-to-four numeric component format. Components are clamped to Chrome's
// documented 16-bit range.
func chromeVersion(version string) string {
	var components []string
	for i := 0; i < len(version) && len(components) < 4; {
		if !unicode.IsDigit(rune(version[i])) {
			i++
			continue
		}
		start := i
		for i < len(version) && unicode.IsDigit(rune(version[i])) {
			i++
		}
		n, _ := strconv.ParseUint(version[start:i], 10, 16)
		if n > 65535 {
			n = 65535
		}
		components = append(components, strconv.FormatUint(n, 10))
	}
	for len(components) < 3 {
		components = append(components, "0")
	}

	return strings.Join(components, ".")
}

// Filename returns a download-safe name carrying the numeric extension version.
func Filename(version string) string {
	return "krabby-browser-extension-" + chromeVersion(version) + ".zip"
}

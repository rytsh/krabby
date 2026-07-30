package browserextension

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestArchiveVersionsManifestForRunningBuild(t *testing.T) {
	data, err := Archive("v1.7.2-14-gabc123")
	if err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var manifestFile *zip.File
	var iconFound bool
	for _, file := range zr.File {
		if file.Name == archiveRoot+"/manifest.json" {
			manifestFile = file
		}
		if file.Name == archiveRoot+"/icons/icon128.png" {
			iconFound = true
		}
	}
	if manifestFile == nil {
		t.Fatal("manifest.json missing from extension archive")
	}
	if !iconFound {
		t.Fatal("extension icon missing from archive")
	}

	r, err := manifestFile.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	content, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version     string `json:"version"`
		VersionName string `json:"version_name"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1.7.2.14" || manifest.VersionName != "v1.7.2-14-gabc123" {
		t.Fatalf("manifest version = %q (%q)", manifest.Version, manifest.VersionName)
	}
}

func TestChromeVersion(t *testing.T) {
	for input, want := range map[string]string{
		"v0.0.0":          "0.0.0",
		"v2.5.1-rc.3":     "2.5.1.3",
		"development":     "0.0.0",
		"v70000.1.2":      "65535.1.2",
		"release-1.2.3.4": "1.2.3.4",
	} {
		if got := chromeVersion(input); got != want {
			t.Errorf("chromeVersion(%q) = %q, want %q", input, got, want)
		}
	}
}

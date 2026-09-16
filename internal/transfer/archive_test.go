package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"
)

func TestArchiveRejectsUnsafeMembers(t *testing.T) {
	for _, hdr := range []tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/tmp/escape", Typeflag: tar.TypeReg},
		{Name: "files/repos/../../escape", Typeflag: tar.TypeReg},
		{Name: "databases/state", Typeflag: tar.TypeSymlink, Linkname: "../files/repos"},
		{Name: "files/repos/link", Typeflag: tar.TypeSymlink, Linkname: "../../../outside"},
		{Name: "files/repos/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "files/repos/link", Typeflag: tar.TypeLink, Linkname: "files/docs/a"},
	} {
		t.Run(hdr.Name+hdr.Linkname, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			check(t, tw.WriteHeader(&hdr))
			check(t, tw.Close())
			check(t, gz.Close())
			if _, err := readArchive(context.Background(), &buf, t.TempDir()); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}

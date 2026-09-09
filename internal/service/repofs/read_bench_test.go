package repofs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkReadFileSmall(b *testing.B) {
	dir := b.TempDir()
	content := strings.Repeat("func example() {}\n", 60)
	if err := os.WriteFile(filepath.Join(dir, "small.go"), []byte(content), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		file, err := ReadFile(dir, "small.go", 0, 0)
		if err != nil || file.Content != content || file.Truncated {
			b.Fatalf("read err=%v file=%+v", err, file)
		}
	}
}

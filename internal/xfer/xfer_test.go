package xfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "data", "nested"), 0o755)
	os.WriteFile(filepath.Join(src, "data", "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(src, "data", "nested", "b.txt"), []byte("beta"), 0o644)

	var buf bytes.Buffer
	if err := Tar(&buf, src, []string{"data"}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Untar(&buf, dst); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"data/a.txt":        "alpha",
		"data/nested/b.txt": "beta",
	} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// The important one: a crafted archive must not write outside its destination.
func TestUntarRefusesPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../escaped.txt",
		"../../escaped.txt",
		"data/../../escaped.txt",
		"/absolute.txt",
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			body := []byte("pwned")
			tw.WriteHeader(&tar.Header{
				Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
			})
			tw.Write(body)
			tw.Close()
			gz.Close()

			parent := t.TempDir()
			dst := filepath.Join(parent, "dest")
			err := Untar(&buf, dst)

			// Either it refused, or it wrote strictly inside dest. Never outside.
			if err == nil {
				if _, statErr := os.Stat(filepath.Join(parent, "escaped.txt")); statErr == nil {
					t.Fatalf("entry %q escaped the destination", name)
				}
			} else if !strings.Contains(err.Error(), "escapes the destination") {
				t.Fatalf("unexpected error for %q: %v", name, err)
			}
			// And nothing may exist above dest under any circumstances.
			if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); err == nil {
				t.Fatalf("entry %q wrote outside the destination", name)
			}
		})
	}
}

func TestUntarSkipsSymlinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// A symlink to /etc would let a later entry write through it.
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
	tw.Close()
	gz.Close()

	dst := t.TempDir()
	if err := Untar(&buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Error("symlink was extracted; it should be skipped")
	}
}

func TestTarRefusesPathOutsideBase(t *testing.T) {
	base := t.TempDir()
	var buf bytes.Buffer
	if err := Tar(&buf, base, []string{"../../etc/passwd"}); err == nil {
		t.Error("packing a path outside base should be refused")
	}
}

func TestUntarRejectsNonGzip(t *testing.T) {
	if err := Untar(strings.NewReader("not a tarball"), t.TempDir()); err == nil {
		t.Error("expected an error for a non-gzip stream")
	}
}

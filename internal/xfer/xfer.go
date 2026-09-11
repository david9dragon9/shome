// Package xfer packs and unpacks the tar streams shome uses to move job inputs
// and results between the controller and its nodes.
//
// Extraction is the security-sensitive half: a tar archive can name entries
// like "../../etc/thing" or contain symlinks pointing outside the destination.
// Untar refuses both, so a malicious or malformed archive cannot write outside
// the directory it was given.
package xfer

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxEntrySize caps a single file, and MaxTotalSize the whole archive, so a
// decompression bomb cannot fill a node's disk.
const (
	MaxEntrySize = 1 << 30 // 1 GiB
	MaxTotalSize = 4 << 30 // 4 GiB
)

// Tar writes paths (files or directories, relative to base) as a gzipped tar.
func Tar(w io.Writer, base string, paths []string) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, p := range paths {
		full := filepath.Join(base, p)
		// Never follow a path out of base, even at pack time.
		if !within(base, full) {
			return fmt.Errorf("refusing to pack %q: outside %s", p, base)
		}
		err := filepath.Walk(full, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// Skip anything that is not a regular file or directory: device
			// nodes and sockets have no meaning on the far side, and symlinks
			// are a traversal risk.
			if !fi.Mode().IsRegular() && !fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if fi.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if fi.IsDir() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if err != nil {
			return fmt.Errorf("pack %s: %w", p, err)
		}
	}
	return nil
}

// Untar extracts a gzipped tar into dest, refusing any entry that would escape.
func Untar(r io.Reader, dest string) error {
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absDest, 0o700); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a valid gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Reject absolute paths and any ".." component before touching disk.
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing archive entry %q: escapes the destination", hdr.Name)
		}
		target := filepath.Join(absDest, name)
		// Belt and braces: verify the joined path really is inside dest.
		if !within(absDest, target) {
			return fmt.Errorf("refusing archive entry %q: escapes the destination", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > MaxEntrySize {
				return fmt.Errorf("archive entry %q is %d bytes, over the limit", hdr.Name, hdr.Size)
			}
			total += hdr.Size
			if total > MaxTotalSize {
				return fmt.Errorf("archive exceeds the %d byte total limit", int64(MaxTotalSize))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil && err != io.EOF {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks, hardlinks, devices, fifos: all skipped deliberately.
			// A symlink is the classic way to escape an extraction directory.
			continue
		}
	}
}

// within reports whether path is inside base.
func within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

package main

import (
	"archive/zip"
	"compress/flate"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var packageFiles = []string{"BUILDINFO", "LICENSE", "VERSION", "v2node"}

func main() {
	source := flag.String("source", "", "directory containing release files")
	output := flag.String("output", "", "destination ZIP file")
	flag.Parse()
	if *source == "" || *output == "" || flag.NArg() != 0 {
		fatalf("usage: packagezip -source DIR -output FILE")
	}
	if err := createArchive(*source, *output); err != nil {
		fatalf("create archive: %v", err)
	}
}

func createArchive(source, output string) (returnErr error) {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", source)
	}
	sort.Strings(packageFiles)
	temporary := output + ".tmp"
	if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			if err := file.Close(); returnErr == nil && err != nil {
				returnErr = err
			}
		}
		if returnErr != nil {
			_ = os.Remove(temporary)
		}
	}()

	archive := zip.NewWriter(file)
	archive.RegisterCompressor(zip.Deflate, func(writer io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(writer, flate.BestCompression)
	})
	fixedTime := time.Unix(315532800, 0).UTC()
	for _, name := range packageFiles {
		path := filepath.Join(source, name)
		entryInfo, err := os.Lstat(path)
		if err != nil {
			_ = archive.Close()
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			_ = archive.Close()
			return fmt.Errorf("package entry is not a regular file: %s", path)
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetModTime(fixedTime)
		if name == "v2node" {
			header.SetMode(0o755)
		} else {
			header.SetMode(0o644)
		}
		entry, err := archive.CreateHeader(header)
		if err != nil {
			_ = archive.Close()
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			_ = archive.Close()
			return err
		}
		_, copyErr := io.Copy(entry, input)
		closeErr := input.Close()
		if copyErr != nil {
			_ = archive.Close()
			return copyErr
		}
		if closeErr != nil {
			_ = archive.Close()
			return closeErr
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fileClosed = true
	if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(temporary, output)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

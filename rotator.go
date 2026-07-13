package logging

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type fileSink interface {
	io.Writer
	Close() error
}

// secureRotator is intentionally small: unlike the previous dependency it
// returns write and compression errors and explicitly enforces file modes.
type secureRotator struct {
	filename  string
	file      *os.File
	size      int64
	threshold int64
	maxRolls  int
}

func newSecureRotator(filename string, threshold int64, maxRolls int) (*secureRotator, error) {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &secureRotator{filename: filename, file: f, size: info.Size(), threshold: threshold, maxRolls: maxRolls}, nil
}

func (r *secureRotator) Write(p []byte) (int, error) {
	n, err := r.file.Write(p)
	r.size += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, err
	}
	if r.size >= r.threshold && len(p) > 0 && p[len(p)-1] == '\n' {
		if err := r.rotate(); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (r *secureRotator) Close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *secureRotator) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	archive := fmt.Sprintf("%s.%d", r.filename, r.nextArchiveNumber())
	if err := os.Rename(r.filename, archive); err != nil {
		return err
	}
	if err := os.Chmod(archive, 0600); err != nil {
		return err
	}
	f, err := os.OpenFile(r.filename, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	r.file, r.size = f, 0
	if err := gzipArchive(archive); err != nil {
		return err
	}
	if err := r.removeOldArchives(); err != nil {
		return err
	}
	return nil
}

func gzipArchive(name string) (err error) {
	in, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); err == nil {
			err = closeErr
		}
	}()
	outName := name + ".gz"
	out, err := os.OpenFile(outName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = out.Chmod(0600); err != nil {
		_ = out.Close()
		return err
	}
	zw := gzip.NewWriter(out)
	_, copyErr := io.Copy(zw, in)
	closeGzipErr := zw.Close()
	closeFileErr := out.Close()
	if err = errors.Join(copyErr, closeGzipErr, closeFileErr); err != nil {
		return err
	}
	return os.Remove(name)
}

func (r *secureRotator) nextArchiveNumber() int {
	max := 0
	for _, number := range r.archiveNumbers() {
		if number > max {
			max = number
		}
	}
	return max + 1
}

func (r *secureRotator) removeOldArchives() error {
	numbers := r.archiveNumbers()
	sort.Ints(numbers)
	for len(numbers) > r.maxRolls {
		number := numbers[0]
		for _, name := range []string{
			fmt.Sprintf("%s.%d.gz", r.filename, number),
			fmt.Sprintf("%s.%d", r.filename, number),
		} {
			if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		numbers = numbers[1:]
	}
	return nil
}

func (r *secureRotator) archiveNumbers() []int {
	matches, err := filepath.Glob(r.filename + ".*")
	if err != nil {
		return nil
	}
	seen := make(map[int]struct{}, len(matches))
	numbers := make([]int, 0, len(matches))
	prefix := r.filename + "."
	for _, match := range matches {
		value := strings.TrimSuffix(strings.TrimPrefix(match, prefix), ".gz")
		if number, err := strconv.Atoi(value); err == nil {
			if _, ok := seen[number]; !ok {
				seen[number] = struct{}{}
				numbers = append(numbers, number)
			}
		}
	}
	return numbers
}

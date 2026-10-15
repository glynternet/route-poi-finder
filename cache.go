package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

type fileQueryCache struct {
	dir string
	ttl time.Duration
}

func (c fileQueryCache) path(renderedQuery string) string {
	hash := sha1.Sum([]byte(renderedQuery))
	return filepath.Join(c.dir, base64.URLEncoding.EncodeToString(hash[:]))
}

func (c fileQueryCache) Lookup(renderedQuery string) ([]element, bool, error) {
	path := c.path(renderedQuery)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("checking cache file(%s): %w", path, err)
	}
	if age := time.Since(info.ModTime()); age > c.ttl {
		log.Printf("cache expired (age %s > ttl %s): %s", age.Round(time.Second), c.ttl, renderedQuery[:min(80, len(renderedQuery))])
		return nil, false, nil
	}
	elements, err := readCachedElements(path)
	if err != nil {
		return nil, false, err
	}
	if debug {
		log.Printf("query fetched from cached result: %s", path)
	}
	return elements, true, nil
}

func (c fileQueryCache) Store(renderedQuery string, body io.Reader) ([]element, error) {
	path := c.path(renderedQuery)
	if err := atomicSlurp(c.dir, body, path); err != nil {
		return nil, fmt.Errorf("storing content into cache: %w", err)
	}
	if debug {
		log.Printf("query result written: %s", path)
	}
	return readCachedElements(path)
}

func readCachedElements(path string) ([]element, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening cached query state file(%s): %w", path, err)
	}
	var r response
	if err := json.NewDecoder(f).Decode(&r); err != nil {
		_ = f.Close() // Preserve the decoding error.
		return nil, fmt.Errorf("decoding response body: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("closing response body: %w", err)
	}
	return r.Elements, nil
}

func atomicSlurp(cacheDir string, resp io.Reader, path string) error {
	tmpFile, err := os.CreateTemp(cacheDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for cache write: %w", err)
	}
	if _, err := io.Copy(tmpFile, resp); err != nil {
		_ = tmpFile.Close()           // Preserve the write error.
		_ = os.Remove(tmpFile.Name()) // Best-effort cleanup of an incomplete write.
		return fmt.Errorf("writing response body to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name()) // Preserve the close error during cleanup.
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpFile.Name(), path); err != nil {
		_ = os.Remove(tmpFile.Name()) // Preserve the rename error during cleanup.
		return fmt.Errorf("renaming temp file to cache path: %w", err)
	}
	return nil
}

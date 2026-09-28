// Package cache manages the on-disk store of published .sra files: a
// sharded directory layout mirroring NCBI's own SRA repository, and
// atomic (temp-then-rename) publish/remove so a concurrent reader never
// observes a partially written file.
//
// See PLAN-sra-cache-service-implementation.md, Phase 5, and
// PLAN-sra-precache-service.md §2.1/§3 for the design this implements.
package cache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Cache is an on-disk store of published .sra files, rooted at a single
// directory (expected to be local SSD -- see the design doc's §2.1 for why
// the shared NetApp is unsuitable for this).
//
// Layout: <root>/sra/<acc[0:3]>/<acc[0:6]>/<acc>/<acc>.sra
// e.g. SRR12345678 -> sra/SRR/SRR1234/SRR12345678/SRR12345678.sra
// -- the same sharding NCBI's own toolkit cache uses, so the tree at least
// looks familiar to anyone who has poked around ~/ncbi/public/sra.
type Cache struct {
	root string
}

// New returns a Cache rooted at root. root is expected to already exist
// (config validation -- Phase 1/10 -- is responsible for creating/
// validating it); New itself does not touch the filesystem.
func New(root string) *Cache {
	return &Cache{root: root}
}

// Root returns the cache root directory this Cache was constructed with.
func (c *Cache) Root() string {
	return c.root
}

// shardComponents returns the two sharding prefix components for accession,
// clamped to accession's own length. This is what keeps Path/Publish/Remove
// from ever panicking (index out of range) on a pathologically short --
// including empty -- accession: it just degenerates to a shallower, but
// still well-defined, path rather than erroring.
func shardComponents(accession string) (p3, p6 string) {
	p3 = accession
	if len(accession) > 3 {
		p3 = accession[:3]
	}
	p6 = accession
	if len(accession) > 6 {
		p6 = accession[:6]
	}
	return p3, p6
}

// Path computes the expected on-disk path for accession's cached .sra file
// without touching the filesystem. Used both for cache-hit checks and to
// know what Publish/Remove will operate on.
func (c *Cache) Path(accession string) string {
	p3, p6 := shardComponents(accession)
	return filepath.Join(c.root, "sra", p3, p6, accession, accession+".sra")
}

// Publish streams src into accession's cache slot. It writes to a temp file
// in the same directory as the final path -- guaranteeing the same
// filesystem, which is what makes the final os.Rename atomic -- fsyncs and
// closes it, then renames it into place. A concurrent reader opening or
// stat-ing the final path therefore never observes a partially written
// file: it sees either ENOENT or the complete file, never a truncated one.
//
// On any failure the temp file is cleaned up; the final path is left
// untouched (either absent, or whatever was there before).
func (c *Cache) Publish(accession string, src io.Reader) (path string, size int64, err error) {
	if accession == "" {
		return "", 0, fmt.Errorf("cache: publish: empty accession")
	}

	finalPath := c.Path(accession)
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("cache: publish %s: mkdir %s: %w", accession, dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-"+accession+"-*.sra")
	if err != nil {
		return "", 0, fmt.Errorf("cache: publish %s: create temp file: %w", accession, err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	n, err := io.Copy(tmp, src)
	if err != nil {
		return "", 0, fmt.Errorf("cache: publish %s: writing: %w", accession, err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("cache: publish %s: fsync: %w", accession, err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("cache: publish %s: closing temp file: %w", accession, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", 0, fmt.Errorf("cache: publish %s: rename into place: %w", accession, err)
	}

	ok = true
	return finalPath, n, nil
}

// Remove deletes accession's cached file, if present, and then cleans up
// any now-empty shard directories left behind (the <acc>/, <p6>/, <p3>/
// levels), stopping at the shared "sra" root -- so repeated eviction
// doesn't grow a permanently expanding tree of empty directories.
//
// Removing an accession with no cached file is not an error.
func (c *Cache) Remove(accession string) error {
	finalPath := c.Path(accession)
	if err := os.Remove(finalPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cache: remove %s: %w", accession, err)
	}

	shardRoot := filepath.Join(c.root, "sra")
	dir := filepath.Dir(finalPath)
	for dir != shardRoot && strings.HasPrefix(dir+string(filepath.Separator), shardRoot+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			// ENOTEMPTY (another accession shares this shard directory),
			// already gone, or some other issue -- any of those just means
			// "stop climbing", not a real error worth reporting.
			break
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

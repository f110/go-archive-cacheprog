package cacheprog

import (
	"archive/zip"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time
	DiskPath string
}

type pendingEntry struct {
	actionID []byte
	outputID []byte
	size     int64
	diskPath string
}

type ArchiveCache struct {
	archivePath string
	tmpDir      string
	method      Compression

	mu      sync.Mutex
	zr      *zip.ReadCloser
	entries map[string]*zip.File // populated in OpenArchive, read-only afterwards
	pending map[string]pendingEntry

	extractMu sync.Mutex
	extracts  map[string]*extractOnce

	stats      Stats
	flushStats flushStats
}

type flushStats struct {
	written         bool
	archiveSize     int64
	totalEntries    int
	newEntries      int
	replacedEntries int
	duration        time.Duration
}

type Stats struct {
	Gets     atomic.Int64
	Hits     atomic.Int64
	Misses   atomic.Int64
	HitBytes atomic.Int64
	Puts     atomic.Int64
	PutBytes atomic.Int64
}

type extractOnce struct {
	once sync.Once
	err  error
}

func OpenArchive(archivePath string, method Compression) (*ArchiveCache, error) {
	absPath, err := filepath.Abs(archivePath)
	if err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "go-archive-cacheprog-*")
	if err != nil {
		return nil, err
	}

	cache := &ArchiveCache{
		archivePath: absPath,
		tmpDir:      tmpDir,
		method:      method,
		entries:     make(map[string]*zip.File),
		pending:     make(map[string]pendingEntry),
		extracts:    make(map[string]*extractOnce),
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cache, nil
		}
		os.RemoveAll(tmpDir)
		return nil, err
	}
	if fi.Size() == 0 {
		return cache, nil
	}

	zr, err := zip.OpenReader(absPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}
	cache.zr = zr
	for _, f := range zr.File {
		actionHex, _, ok := strings.Cut(f.Name, "-")
		if !ok {
			continue
		}
		cache.entries[actionHex] = f
	}
	return cache, nil
}

func (c *ArchiveCache) Get(actionID []byte) (*Entry, error) {
	c.stats.Gets.Add(1)
	entry, err := c.get(actionID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		c.stats.Misses.Add(1)
		return nil, nil
	}
	c.stats.Hits.Add(1)
	c.stats.HitBytes.Add(entry.Size)
	return entry, nil
}

func (c *ArchiveCache) get(actionID []byte) (*Entry, error) {
	key := hex.EncodeToString(actionID)

	c.mu.Lock()
	if p, ok := c.pending[key]; ok {
		diskPath := p.diskPath
		outputID := append([]byte(nil), p.outputID...)
		size := p.size
		c.mu.Unlock()

		fi, err := os.Stat(diskPath)
		if err != nil {
			return nil, err
		}
		return &Entry{OutputID: outputID, Size: size, Time: fi.ModTime(), DiskPath: diskPath}, nil
	}
	f, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return nil, nil
	}

	_, outputHex, _ := strings.Cut(f.Name, "-")
	outputID, err := hex.DecodeString(outputHex)
	if err != nil {
		return nil, fmt.Errorf("invalid output id in archive entry %q: %w", f.Name, err)
	}

	diskPath := filepath.Join(c.tmpDir, "get-"+key)
	if err := c.extract(key, f, diskPath); err != nil {
		return nil, err
	}

	return &Entry{
		OutputID: outputID,
		Size:     int64(f.UncompressedSize64),
		Time:     f.Modified,
		DiskPath: diskPath,
	}, nil
}

func (c *ArchiveCache) extract(key string, f *zip.File, dst string) error {
	c.extractMu.Lock()
	ex, ok := c.extracts[key]
	if !ok {
		ex = &extractOnce{}
		c.extracts[key] = ex
	}
	c.extractMu.Unlock()

	ex.once.Do(func() {
		if _, err := os.Stat(dst); err == nil {
			return
		}
		ex.err = extractEntry(f, dst)
	})
	return ex.err
}

func extractEntry(f *zip.File, dst string) (err error) {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()

	_, err = io.Copy(out, rc)
	return err
}

func (c *ArchiveCache) Put(actionID, outputID []byte, size int64, body []byte) (string, error) {
	if int64(len(body)) != size {
		return "", fmt.Errorf("body size mismatch: got %d, want %d", len(body), size)
	}

	key := hex.EncodeToString(actionID)
	diskPath := filepath.Join(c.tmpDir, "put-"+key)
	if err := os.WriteFile(diskPath, body, 0o644); err != nil {
		return "", err
	}

	c.mu.Lock()
	c.pending[key] = pendingEntry{
		actionID: append([]byte(nil), actionID...),
		outputID: append([]byte(nil), outputID...),
		size:     size,
		diskPath: diskPath,
	}
	c.mu.Unlock()

	c.stats.Puts.Add(1)
	c.stats.PutBytes.Add(size)

	return diskPath, nil
}

func (c *ArchiveCache) WriteStats(w io.Writer) error {
	gets := c.stats.Gets.Load()
	hits := c.stats.Hits.Load()
	misses := c.stats.Misses.Load()
	hitBytes := c.stats.HitBytes.Load()
	puts := c.stats.Puts.Load()
	putBytes := c.stats.PutBytes.Load()

	var hitRate float64
	if total := hits + misses; total > 0 {
		hitRate = float64(hits) / float64(total) * 100
	}

	if _, err := fmt.Fprintf(w,
		"go-archive-cacheprog: cache stats\n"+
			"  archive:       %s\n"+
			"  compression:   %s\n",
		c.archivePath,
		c.method,
	); err != nil {
		return err
	}
	if c.flushStats.written {
		if _, err := fmt.Fprintf(w,
			"  archive size:  %s\n"+
				"  entries:       %d total (new: %d, replaced: %d)\n"+
				"  update time:   %s\n",
			humanBytes(c.flushStats.archiveSize),
			c.flushStats.totalEntries,
			c.flushStats.newEntries,
			c.flushStats.replacedEntries,
			c.flushStats.duration.Round(time.Millisecond),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w,
		"  gets:          %d (hits: %d, misses: %d)\n"+
			"  hit rate:      %.1f%%\n"+
			"  hit bytes:     %s\n"+
			"  puts:          %d (%s)\n",
		gets, hits, misses,
		hitRate,
		humanBytes(hitBytes),
		puts, humanBytes(putBytes),
	)
	return err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (c *ArchiveCache) Close() error {
	flushErr := c.Flush()

	c.mu.Lock()
	if c.zr != nil {
		c.zr.Close()
		c.zr = nil
	}
	c.mu.Unlock()

	rmErr := os.RemoveAll(c.tmpDir)

	if flushErr != nil {
		return flushErr
	}
	return rmErr
}

func (c *ArchiveCache) Flush() error {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]pendingEntry)
	c.mu.Unlock()

	archiveExists := true
	if _, err := os.Stat(c.archivePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		archiveExists = false
	}
	if len(pending) == 0 && archiveExists {
		return nil
	}

	start := time.Now()

	archiveDir := filepath.Dir(c.archivePath)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(archiveDir, ".tmp-archive-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	zw := zip.NewWriter(tmpFile)

	var keptCount, replacedCount int
	if c.zr != nil {
		for _, f := range c.zr.File {
			if actionHex, _, ok := strings.Cut(f.Name, "-"); ok {
				if _, replace := pending[actionHex]; replace {
					replacedCount++
					continue
				}
			}
			if err := zw.Copy(f); err != nil {
				return err
			}
			keptCount++
		}
	}

	now := time.Now()
	for _, p := range pending {
		name := fmt.Sprintf("%s-%s", hex.EncodeToString(p.actionID), hex.EncodeToString(p.outputID))
		header := &zip.FileHeader{Name: name, Method: uint16(c.method), Modified: now}
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if err := copyFile(w, p.diskPath); err != nil {
			return err
		}
	}

	if err := zw.Close(); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.zr != nil {
		c.zr.Close()
		c.zr = nil
	}
	c.mu.Unlock()

	if err := os.Rename(tmpPath, c.archivePath); err != nil {
		return err
	}
	committed = true

	c.flushStats = flushStats{
		written:         true,
		totalEntries:    keptCount + len(pending),
		newEntries:      len(pending),
		replacedEntries: replacedCount,
		duration:        time.Since(start),
	}
	if fi, err := os.Stat(c.archivePath); err == nil {
		c.flushStats.archiveSize = fi.Size()
	}
	return nil
}

func copyFile(dst io.Writer, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(dst, src)
	return err
}

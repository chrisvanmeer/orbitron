package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"orbitron/internal/build"
	"orbitron/internal/httputil"
	"orbitron/internal/inventory"
	"orbitron/internal/logger"
)

const (
	// dumpRoot is the single top-level directory every archived entry lives
	// under, so extracting the archive never litters the destination.
	dumpRoot = "orbitron"
	// dumpManifestName is the archive's own inventory. It is written as the
	// *last* entry: hashing every file in a single pass means the checksums are
	// exact, and a download that is interrupted half way simply has no manifest
	// at all, which backup tooling can detect. (A tar reader that extracts
	// normally just ignores it, matching nothing on disk.)
	dumpManifestName = dumpRoot + "/dump.json"
	// dumpFormatVersion identifies the archive layout for restore tooling.
	dumpFormatVersion = 1
	// dumpFlushInterval is how many uncompressed payload bytes are written
	// between explicit flushes. On a slow uplink to a backup host this keeps
	// data moving (and any intermediate proxy from timing the stream out)
	// instead of buffering the whole archive.
	dumpFlushInterval = 32 << 20
)

// dumpManifestFile is one archived regular file with the checksum computed
// while it was streamed into the archive.
type dumpManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// dumpManifest is serialized as <dumpRoot>/dump.json. It lets a backup host
// verify the transfer and know exactly what the mirror held at dump time.
type dumpManifest struct {
	Format             string             `json:"format"`
	Version            int                `json:"version"`
	CreatedAt          string             `json:"created_at"`
	OrbitronVersion    string             `json:"orbitron_version"`
	StoragePath        string             `json:"storage_path"`
	SyncRunning        bool               `json:"sync_running"`
	Excluded           []string           `json:"excluded"`
	Roles              int                `json:"roles"`
	RoleVersions       int                `json:"role_versions"`
	Collections        int                `json:"collections"`
	CollectionVersions int                `json:"collection_versions"`
	Bytes              int64              `json:"bytes"`
	Files              []dumpManifestFile `json:"files"`
}

// dumpArchiver streams the storage tree into a tar.Writer as it walks it,
// recording every file's digest in the manifest.
type dumpArchiver struct {
	ctx      context.Context
	root     string
	excluded map[string]bool
	gw       *gzip.Writer
	tw       *tar.Writer
	flusher  http.Flusher
	manifest *dumpManifest

	// payload counts the bytes of file content archived (not tar/gzip
	// overhead), used for flush pacing and progress logging.
	payload int64
	flushed int64
}

// flush pushes buffered archive data to the client every dumpFlushInterval
// bytes, or unconditionally when force is set (end of stream).
func (a *dumpArchiver) flush(force bool) {
	if !force && a.payload-a.flushed < dumpFlushInterval {
		return
	}
	// gzip.Flush emits a self-contained (resumable) deflate block; the client
	// keeps accumulating a valid stream. tar blocks need no explicit flush here
	// since we only flush at entry boundaries.
	_ = a.gw.Flush()
	if a.flusher != nil {
		a.flusher.Flush()
	}
	a.flushed = a.payload
}

func (a *dumpArchiver) addHeader(hdr *tar.Header) error {
	if err := a.tw.WriteHeader(hdr); err != nil {
		return err
	}
	return nil
}

// writeDir archives an (empty) directory entry so the extracted tree keeps its
// shape even for roles whose version directory is intentionally empty.
func (a *dumpArchiver) writeDir(name string, info os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	return a.addHeader(hdr)
}

// writeSymlink archives a symbolic link verbatim, preserving its target. The
// target is read explicitly: tar.FileInfoHeader does not do that for us.
func (a *dumpArchiver) writeSymlink(name, target string, info os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, target)
	if err != nil {
		return err
	}
	hdr.Name = name
	return a.addHeader(hdr)
}

// writeFile streams one regular file into the archive, hashing it on the way.
//
// The header is taken from an fstat of the opened descriptor rather than from
// the directory walk: if a concurrent sync replaced the file in between, the
// declared size and the bytes that follow still agree. A file that changes
// underneath an open descriptor is caught by the explicit size check, which
// fails the dump with an actionable message instead of a corrupt archive.
func (a *dumpArchiver) writeFile(name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if !info.Mode().IsRegular() {
		// The path was something else (directory, socket, ...) by the time we
		// opened it. Leave it out rather than archive a dangling reference.
		_ = f.Close()
		return nil
	}

	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		_ = f.Close()
		return err
	}
	hdr.Name = name
	if err := a.addHeader(hdr); err != nil {
		_ = f.Close()
		return err
	}

	h := sha256.New()
	written, copyErr := copyContext(a.ctx, io.MultiWriter(a.tw, h), f)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != info.Size() {
		return fmt.Errorf("file %s changed size while archiving (%d of %d bytes): the cache is being written to concurrently, retry the dump when no sync is running", path, written, info.Size())
	}

	a.payload += written
	a.manifest.Files = append(a.manifest.Files, dumpManifestFile{
		Path:   name,
		Size:   written,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	})
	a.flush(false)
	return nil
}

// writeTree walks the storage root in lexical order (deterministic archives)
// and archives every directory, file and symlink it finds.
func (a *dumpArchiver) writeTree() error {
	return filepath.Walk(a.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == a.root {
			return nil
		}
		if cerr := a.ctx.Err(); cerr != nil {
			return cerr
		}
		if a.excluded[path] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(a.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
			return fmt.Errorf("refusing to archive %q: it escapes the storage root %s", path, a.root)
		}
		name := dumpRoot + "/" + rel

		switch {
		case info.IsDir():
			return a.writeDir(name, info)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return a.writeSymlink(name, target, info)
		case info.Mode().IsRegular():
			return a.writeFile(name, path)
		default:
			// Sockets, fifos and device nodes have no meaningful backup form.
			logger.Warn("Cache dump: skipping non-regular file %s (%s)", path, info.Mode())
			return nil
		}
	})
}

// writeManifest appends dump.json as the final archive entry.
func (a *dumpArchiver) writeManifest(created time.Time) error {
	a.manifest.Bytes = a.payload
	data, err := json.MarshalIndent(a.manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	hdr := &tar.Header{
		Name:     dumpManifestName,
		Typeflag: tar.TypeReg,
		Mode:     0o600,
		Size:     int64(len(data)),
		ModTime:  created,
	}
	if err := a.addHeader(hdr); err != nil {
		return err
	}
	if _, err := a.tw.Write(data); err != nil {
		return err
	}
	a.payload += int64(len(data))
	a.flush(true)
	return nil
}

// dumpExclusions returns the absolute paths that must never appear in a dump.
//
// The token store is the important one: in the container layout it lives
// *inside* the storage path (tokens_file: /data/tokens.json), so a naive tree
// dump would ship administrative bearer tokens into the backup host and the
// offline vault. There is deliberately no option to include it.
//
// Both the configured token store and the container-convention path
// <storage>/tokens.json are filtered, so a store sitting in the storage root is
// left out even if the running configuration points somewhere else.
func (s *Server) dumpExclusions(root string) (map[string]bool, []string) {
	excluded := make(map[string]bool)
	var listed []string

	candidates := []string{filepath.Join(root, "tokens.json")}
	if s.cfg.TokensFile != "" {
		candidates = append(candidates, s.cfg.TokensFile)
	}

	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil || !pathWithinRoot(root, abs) || excluded[abs] {
			continue
		}
		excluded[abs] = true
		if rel, rerr := filepath.Rel(root, abs); rerr == nil {
			listed = append(listed, filepath.ToSlash(rel))
		} else {
			listed = append(listed, abs)
		}
	}

	// Scratch file the access recorder writes through (atomic tmp + rename).
	tmp := filepath.Join(root, ".access.json.tmp")
	excluded[tmp] = true
	listed = append(listed, ".access.json.tmp")

	return excluded, listed
}

// pathWithinRoot reports whether p is root itself or lives below it.
func pathWithinRoot(root, p string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// newDumpGzipWriter builds the gzip stream for a dump. The default level suits
// archival; "fast" trades ratio for CPU, which matters when dumping a large
// mirror to a backup host on a tight schedule.
func newDumpGzipWriter(w io.Writer, compress string) (*gzip.Writer, error) {
	switch compress {
	case "", "default":
		return gzip.NewWriterLevel(w, gzip.DefaultCompression)
	case "fast":
		return gzip.NewWriterLevel(w, gzip.BestSpeed)
	default:
		return nil, fmt.Errorf("unsupported compress value %q: use \"default\" or \"fast\"", compress)
	}
}

// copyContext copies src to dst, honouring context cancellation so a client
// that walks away from a multi-gigabyte dump stops the walk promptly.
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// HandleDump streams the entire cached content as a single gzip-compressed tar
// archive, suitable for shipping to a backup machine and archiving offline.
//
// The archive contains everything under the storage path — roles, collection
// archives, git-sourced collection checkouts, stored requirements manifests and
// the access index — except the token store, which is always left out so a
// content backup can never leak administrative credentials. An authenticated
// caller is required; the route is registered behind AuthMiddleware.
//
// The stream is generated on the fly: nothing is staged on disk, so a dump
// never needs scratch space proportional to the mirror. The trade-off is that
// there is no Content-Length and an interrupted transfer yields a truncated
// archive. The per-file SHA-256 checksums in <dumpRoot>/dump.json (the final
// entry) let the receiving side verify a completed transfer and detect a torn
// one.
func (s *Server) HandleDump(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	root, err := filepath.Abs(s.cfg.StoragePath)
	if err != nil {
		logger.Error("Cache dump: storage path %q is not resolvable: %v", s.cfg.StoragePath, err)
		http.Error(w, "storage path is not resolvable", http.StatusInternalServerError)
		return
	}

	inv := inventory.Scan(s.cfg.StoragePath)
	syncRunning := false
	if s.fetcher != nil {
		if snap := s.fetcher.StatusSnapshot(); snap.Current != nil && snap.Current.Status == "running" {
			syncRunning = true
		}
	}

	excluded, excludedList := s.dumpExclusions(root)

	// Resolve the compression level before any header is written so an invalid
	// value can still produce a clean 400.
	flusher, _ := w.(http.Flusher)
	gw, err := newDumpGzipWriter(w, r.URL.Query().Get("compress"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tw := tar.NewWriter(gw)

	created := time.Now().UTC()
	filename := fmt.Sprintf("orbitron-dump-%s.tar.gz", created.Format("20060102T150405Z"))

	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Orbitron-Dump-Format", strconv.Itoa(dumpFormatVersion))
	h.Set("X-Orbitron-Dump-Created", created.Format(time.RFC3339))
	// Approximate uncompressed payload (roles + collections, block-allocated)
	// so a caller can show progress. The exact figure is in dump.json.
	h.Set("X-Orbitron-Dump-Bytes", strconv.FormatInt(inv.StorageBytes(), 10))
	if syncRunning {
		h.Set("X-Orbitron-Dump-Sync", "running")
	} else {
		h.Set("X-Orbitron-Dump-Sync", "idle")
	}

	logger.Info("Cache dump requested by %s (%s): %d roles/%d collections, ~%d bytes, sync=%v",
		httputil.ClientIP(r), filename, inv.RoleCount(), inv.CollectionCount(), inv.StorageBytes(), syncRunning)

	archiver := &dumpArchiver{
		ctx:      r.Context(),
		root:     root,
		excluded: excluded,
		gw:       gw,
		tw:       tw,
		flusher:  flusher,
		manifest: &dumpManifest{
			Format:             "orbitron-dump",
			Version:            dumpFormatVersion,
			CreatedAt:          created.Format(time.RFC3339),
			OrbitronVersion:    build.Version,
			StoragePath:        s.cfg.StoragePath,
			SyncRunning:        syncRunning,
			Excluded:           excludedList,
			Roles:              inv.RoleCount(),
			RoleVersions:       inv.RoleVersionCount(),
			Collections:        inv.CollectionCount(),
			CollectionVersions: inv.CollectionVersionCount(),
			Files:              []dumpManifestFile{},
		},
	}

	writeErr := archiver.writeTree()
	if writeErr == nil {
		writeErr = archiver.writeManifest(created)
	}
	if writeErr != nil {
		// The status line and part of the body are already on the wire, so a
		// clean error response is no longer possible. Return without closing
		// the gzip stream: the missing footer makes the truncation detectable
		// on the receiving side.
		if r.Context().Err() != nil {
			logger.Info("Cache dump aborted by client disconnect after %d bytes", archiver.payload)
		} else {
			logger.Error("Cache dump failed after %d bytes: %v", archiver.payload, writeErr)
		}
		return
	}

	// Close in order: tar first (writes the end-of-archive marker), then gzip
	// (writes the footer). Both must succeed for the archive to be complete.
	if err := tw.Close(); err != nil {
		logger.Error("Cache dump: closing tar stream failed: %v", err)
		return
	}
	if err := gw.Close(); err != nil {
		logger.Error("Cache dump: closing gzip stream failed: %v", err)
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	logger.Info("Cache dump completed (%s): %d files, %d uncompressed bytes", filename, len(archiver.manifest.Files), archiver.payload)
}

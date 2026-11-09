package engine

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/storage"
)

// Host API for an autoscaled sandbox host: one admission check, leases, the
// state archive, drain and the stats a controller places sandboxes by.

// archiveTimeout bounds one archive. It matches how long the backend waits on
// an "archiving" sandbox before it restores from the previous generation.
var archiveTimeout = 10 * time.Minute

// discardTimeout bounds removing a container whose create failed.
const discardTimeout = 30 * time.Second

// defaultStateMaxBytes caps one archive when the config leaves it unset.
const defaultStateMaxBytes = 2 << 30

// zstd limits: one goroutine each way and a bounded window, so a hostile or
// corrupt archive cannot take the host's memory.
const zstdMaxWindow = 64 << 20

var errStateTooLarge = errors.New("state exceeds sandbox.state.max_bytes")

// admit is the single admission check for create, start and snapshot restore,
// under e.mu. A start only checks draining and pressure, since a stopped
// container already holds its slot. A new container also needs a free slot and
// takes it: count covers every container that exists and drops only after its
// container is removed; creating counts the ones not yet started.
func (e *Engine) admit(start bool, sc *runtime.StorageConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.draining {
		return ErrDraining
	}
	if e.pressureMonitor.CurrentEvent().Level >= PressureCritical {
		return ErrPressureTooHigh
	}
	if start {
		return nil
	}
	if e.count >= e.config.MaxSandboxes {
		return ErrLimitReached
	}
	// Validate storage under the lock, as before, for the shared volume check.
	if err := e.validateStorage(sc); err != nil {
		return fmt.Errorf("invalid storage config: %w", err)
	}
	e.count++
	e.creating++
	return nil
}

func (e *Engine) doneCreating() {
	e.mu.Lock()
	e.creating--
	e.mu.Unlock()
}

// discardContainer removes a container that never became a running sandbox.
// Its slot is released only once it is gone; if the remove fails it stays
// tracked as never started (destroyed, not archived, by the reaper or DELETE).
// The remove outlives the request that failed.
func (e *Engine) discardContainer(ctx context.Context, sb *Sandbox) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	if err := e.runtime.Remove(ctx, sb.ID); err != nil {
		e.logger.Warn("failed to remove a sandbox that did not start, keeping it tracked", "id", sb.ID, "error", err)
		sb.markNeverStarted()
		e.sandboxes.Store(sb.ID, sb)
		e.saveQuietly(sb)
		return
	}
	e.decrementCount()
}

// validateState checks a create's state target against the server's state
// config: the bucket is the server's (a request may repeat it, never change
// it) and both keys must sit under the server's key prefix.
func (e *Engine) validateState(cfg *runtime.SandboxConfig) error {
	st := cfg.State
	if st == nil {
		return nil
	}
	sc := e.config.State
	switch {
	case sc.Bucket == "":
		return fmt.Errorf("%w: sandbox state is not configured on this server", ErrInvalidState)
	case st.Bucket != "" && st.Bucket != sc.Bucket:
		return fmt.Errorf("%w: bucket must be the server's state bucket", ErrInvalidState)
	case st.ArchiveKey == "":
		return fmt.Errorf("%w: archive_key is required", ErrInvalidState)
	case st.ArchiveKey == st.RestoreKey:
		return fmt.Errorf("%w: archive_key must differ from restore_key", ErrInvalidState)
	case len(st.Paths) == 0:
		return fmt.Errorf("%w: paths is required", ErrInvalidState)
	case !cfg.WritableRootfs:
		// Docker refuses to copy into a read-only root filesystem.
		return fmt.Errorf("%w: state needs writable_rootfs", ErrInvalidState)
	}
	for _, key := range []string{st.ArchiveKey, st.RestoreKey} {
		if key == "" {
			continue
		}
		if !strings.HasPrefix(key, sc.KeyPrefix) || strings.HasPrefix(key, "/") ||
			strings.Contains(key, "..") || strings.Contains(key, `\`) {
			return fmt.Errorf("%w: key %q must start with %q and contain no \"..\", leading \"/\" or backslash", ErrInvalidState, key, sc.KeyPrefix)
		}
	}
	for _, p := range st.Paths {
		if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
			return fmt.Errorf("%w: path %q must be an absolute, clean directory other than /", ErrInvalidState, p)
		}
	}
	target := *st
	target.Bucket = sc.Bucket
	cfg.State = &target
	return nil
}

func (e *Engine) stateMaxBytes() int64 {
	if e.config.State.MaxBytes > 0 {
		return e.config.State.MaxBytes
	}
	return defaultStateMaxBytes
}

// stateTempDir holds archives while they upload: the configured directory
// next to the store, created on demand.
func (e *Engine) stateTempDir() (string, error) {
	dir := e.config.State.TempDir
	if dir == "" {
		return os.TempDir(), nil
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// sweepStateTemp removes archive temp files a crash left behind.
func (e *Engine) sweepStateTemp() {
	if e.config.State.TempDir == "" {
		return
	}
	stale, _ := filepath.Glob(filepath.Join(e.config.State.TempDir, "guino-state-*"))
	for _, f := range stale {
		_ = os.Remove(f)
	}
}

// stateClient builds an S3 client from the server's S3 settings. With no
// access key it uses the SDK's default chain (the instance role on a host).
func (e *Engine) stateClient(ctx context.Context) (*storage.S3Client, error) {
	region := e.s3Config.Region
	if region == "" {
		region = "us-east-1"
	}
	return storage.NewS3Client(ctx, &storage.S3Credentials{
		Endpoint:              e.s3Config.Endpoint,
		Region:                region,
		AccessKey:             e.s3Config.AccessKey,
		SecretKey:             e.s3Config.SecretKey,
		AllowInternalEndpoint: e.s3Config.AllowInternalEndpoint,
	}, e.logger)
}

// restoreState copies the archive at RestoreKey into a created container,
// before it starts. The download is bounded by max_bytes, and the tar is
// filtered on the way in exactly as on the way out.
func (e *Engine) restoreState(ctx context.Context, id string, st *runtime.StateTarget) error {
	client, err := e.stateClient(ctx)
	if err != nil {
		return err
	}
	body, _, err := client.Download(ctx, st.Bucket, st.RestoreKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return fmt.Errorf("%w: %w", ErrRestoreKeyNotFound, err)
		}
		return err
	}
	defer func() { _ = body.Close() }()
	zr, err := zstd.NewReader(&capReader{r: body, left: e.stateMaxBytes()},
		zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(zstdMaxWindow))
	if err != nil {
		return err
	}
	defer zr.Close()

	pr, pw := io.Pipe()
	done := make(chan struct{})
	var gerr error
	go func() {
		defer close(done)
		tw := tar.NewWriter(pw)
		err := newStateFilter(st.Paths, e.stateMaxBytes()).copy(tar.NewReader(zr), tw, "")
		if err == nil {
			err = tw.Close()
		}
		gerr = err
		pw.CloseWithError(err)
	}()
	err = e.runtime.CopyTo(ctx, id, "/", pr)
	// If the copy stopped early, unblock the goroutine on both sides (its
	// writes, and a read stuck on a slow download) and wait for it: zr is
	// closed on return and must not be read after.
	_ = pr.CloseWithError(err)
	_ = body.Close()
	<-done
	// The filter's error (a refused or oversized entry) wins over a copy that
	// reported success on what it was given.
	if err == nil {
		err = gerr
	}
	return err
}

// LeaseSandbox keeps a sandbox from idling out until now+ttl (see Sandbox.Lease)
// and persists it, so a Guino restart keeps the lease.
func (e *Engine) LeaseSandbox(id string, ttl time.Duration) (time.Time, error) {
	sb, err := e.GetSandbox(id)
	if err != nil {
		return time.Time{}, err
	}
	until, err := sb.Lease(ttl)
	if err != nil {
		return time.Time{}, err
	}
	e.saveQuietly(sb)
	return until, nil
}

// ArchiveSandbox saves the state paths to the archive key and removes the
// sandbox. The API and the idle reaper share this path. Unless forced, a live
// lease or a held operation refuses with ErrInUse.
//
// If the upload fails, the container is unpaused and the sandbox is running
// again. Once the archive began it is persisted as archiving, so after a crash
// it never runs again: the reaper redoes the upload and the remove.
func (e *Engine) ArchiveSandbox(ctx context.Context, id string, force bool) error {
	sb, err := e.GetSandbox(id)
	if err != nil {
		return err
	}
	if sb.Config.State == nil {
		return ErrNoState
	}
	// A forced archive is the explicit way to retry one that failed for good
	// (too large); the reaper leaves those alone.
	if force && sb.claimStuck() {
		return e.resumeArchive(sb)
	}
	prev, err := sb.beginArchive(force)
	if err != nil {
		return err
	}
	e.saveQuietly(sb)

	// A caller that gives up must not cut an upload in half.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), archiveTimeout)
	defer cancel()
	paused, err := e.pauseAndUpload(ctx, sb)
	if err == nil {
		return e.finishArchive(ctx, sb)
	}
	err = fmt.Errorf("archiving state: %w", err)
	if errors.Is(err, errStateTooLarge) {
		// Retrying cannot help: it stays archiving (frozen) until DELETE or a
		// forced archive settles it.
		e.logger.Error("sandbox state is over sandbox.state.max_bytes, not retrying", "id", sb.ID, "error", err)
		sb.markStuck()
		e.saveQuietly(sb)
		return err
	}
	if paused {
		// The archive's own context may be what ran out.
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), reapTimeout)
		uerr := e.runtime.Unpause(uctx, sb.ID)
		ucancel()
		if uerr != nil {
			// Still frozen, so not usable: it stays archiving and the reaper
			// resumes the archive.
			e.logger.Warn("failed to unpause after a failed archive, the reaper resumes it", "id", sb.ID, "error", uerr)
			sb.failed()
			sb.release()
			e.saveQuietly(sb)
			return err
		}
	}
	sb.abortArchive(prev)
	sb.failed()
	e.saveQuietly(sb)
	return err
}

// resumeArchive finishes an archive the reaper (or a forced archive) claimed:
// the upload again if it never completed (forced: the sandbox is archiving
// whatever its lease), then the remove. A failure leaves it archiving, backed
// off, or stuck if the state is too large.
func (e *Engine) resumeArchive(sb *Sandbox) error {
	ctx, cancel := context.WithTimeout(context.Background(), archiveTimeout)
	defer cancel()
	if !sb.uploaded() {
		if _, err := e.pauseAndUpload(ctx, sb); err != nil {
			if errors.Is(err, errStateTooLarge) {
				e.logger.Error("sandbox state is over sandbox.state.max_bytes, not retrying", "id", sb.ID, "error", err)
				sb.markStuck()
			} else {
				sb.failed()
				sb.release()
			}
			e.saveQuietly(sb)
			return fmt.Errorf("archiving state: %w", err)
		}
	}
	return e.finishArchive(ctx, sb)
}

func (e *Engine) finishArchive(ctx context.Context, sb *Sandbox) error {
	sb.markUploaded()
	e.saveQuietly(sb)
	e.logger.Info("sandbox state archived", "id", sb.ID, "key", sb.Config.State.ArchiveKey)
	return e.removeArchived(ctx, sb)
}

// removeArchived removes the container of an uploaded archive. The caller
// holds the sandbox's busy flag. The slot is released only after the remove.
func (e *Engine) removeArchived(ctx context.Context, sb *Sandbox) error {
	ctx, cancel := context.WithTimeout(ctx, reapTimeout)
	defer cancel()
	_ = e.runtime.Stop(ctx, sb.ID, time.Second)
	if err := e.runtime.Remove(ctx, sb.ID); err != nil {
		sb.failed()
		sb.release()
		return fmt.Errorf("removing archived sandbox: %w", err)
	}
	e.sandboxes.Delete(sb.ID)
	if err := e.store.DeleteSandbox(sb.ID); err != nil {
		e.logger.Warn("failed to delete sandbox from store", "id", sb.ID, "error", err)
	}
	e.decrementCount()
	e.logger.Info("archived sandbox removed", "id", sb.ID)
	return nil
}

func (e *Engine) saveQuietly(sb *Sandbox) {
	if err := e.saveSandbox(sb); err != nil {
		e.logger.Warn("failed to save sandbox to store", "id", sb.ID, "error", err)
	}
}

// pauseAndUpload freezes the container so the copy is consistent, then
// uploads. A container that cannot be paused because it is not running
// (stopped, dead, or still paused from before a restart) is already still.
// paused reports whether this call paused it.
func (e *Engine) pauseAndUpload(ctx context.Context, sb *Sandbox) (paused bool, err error) {
	if perr := e.runtime.Pause(ctx, sb.ID); perr == nil {
		paused = true
	} else {
		// An archive of a vanished container would be empty and would replace
		// the last good generation, so the container has to be there.
		info, ierr := e.runtime.Info(ctx, sb.ID)
		if ierr != nil {
			return false, ierr
		}
		if info.Status == runtime.StatusRunning {
			return false, fmt.Errorf("pausing: %w", perr)
		}
	}
	return paused, e.uploadState(ctx, sb)
}

// uploadState writes the state paths as one tar.zst and uploads it with
// If-None-Match, so an archive key is written once. A 412 means an earlier
// attempt already finished, which is success.
func (e *Engine) uploadState(ctx context.Context, sb *Sandbox) error {
	st := sb.Config.State
	dir, err := e.stateTempDir()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "guino-state-*.tar.zst")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err := e.writeStateArchive(ctx, sb.ID, st.Paths, f); err != nil {
		return err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	client, err := e.stateClient(ctx)
	if err != nil {
		return err
	}
	err = client.UploadIfAbsent(ctx, st.Bucket, st.ArchiveKey, f, size)
	if errors.Is(err, storage.ErrObjectExists) {
		e.logger.Info("archive key already written by an earlier attempt", "id", sb.ID, "key", st.ArchiveKey)
		return nil
	}
	return err
}

// writeStateArchive copies each path out of the container and joins them
// into one tar, entries named by their absolute path without the leading
// slash, so restoring is a single copy into "/". File sizes are summed
// across all paths against max_bytes.
func (e *Engine) writeStateArchive(ctx context.Context, id string, paths []string, w io.Writer) error {
	zw, err := zstd.NewWriter(w, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	defer func() { _ = zw.Close() }() // a no-op after the Close below
	tw := tar.NewWriter(zw)
	filter := newStateFilter(paths, e.stateMaxBytes())
	for _, p := range paths {
		rc, err := e.runtime.CopyFrom(ctx, id, p)
		if errors.Is(err, runtime.ErrFileNotFound) {
			continue // a path this sandbox never created
		}
		if err != nil {
			return fmt.Errorf("archiving %s: %w", p, err)
		}
		// Docker names entries relative to the path's parent ("root/x" for /root).
		parent := strings.TrimPrefix(path.Dir(p), "/")
		err = filter.copy(tar.NewReader(rc), tw, parent)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("archiving %s: %w", p, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// tarHeaderBytes is what one entry costs beyond its content, counted against
// the budget so an archive of millions of empty files is bounded too.
const tarHeaderBytes = 512

// stateFilter decides, in one place and for both directions, what a state
// archive may hold. Kept: regular files, directories, symlinks, and hard links
// to a regular file already accepted, each under one of the state paths.
// Dropped: devices, FIFOs and other types; duplicates; anything below an
// accepted symlink or file (extracting it would follow the link); setuid and
// setgid bits; PAX records (xattrs, file capabilities). Entry headers and
// file contents count against the budget.
type stateFilter struct {
	paths    []string
	budget   int64
	used     int64
	accepted map[string]byte // cleaned name to typeflag
}

func newStateFilter(paths []string, budget int64) *stateFilter {
	return &stateFilter{paths: paths, budget: budget, accepted: map[string]byte{}}
}

// copy copies the entries of tr that pass into tw, named under parent.
func (f *stateFilter) copy(tr *tar.Reader, tw *tar.Writer, parent string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		f.used += tarHeaderBytes
		if f.used > f.budget {
			return errStateTooLarge
		}
		if !f.keep(hdr, parent) {
			continue
		}
		if hdr.Typeflag == tar.TypeReg {
			f.used += hdr.Size
			if f.used > f.budget {
				return errStateTooLarge
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// keep rewrites hdr for the archive and reports whether it passes.
func (f *stateFilter) keep(hdr *tar.Header, parent string) bool {
	// path.Join cleans dot segments; the result is then held under the state paths.
	name := path.Join(parent, hdr.Name) //nolint:gosec // G305: checked by underStatePaths below
	if !underStatePaths(name, f.paths) {
		return false
	}
	if _, dup := f.accepted[name]; dup {
		return false
	}
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if t, ok := f.accepted[dir]; ok && t != tar.TypeDir {
			return false // below a file or a symlink
		}
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
	case tar.TypeLink:
		target := path.Join(parent, hdr.Linkname) //nolint:gosec // G305: must be an accepted regular file, so under the state paths
		if f.accepted[target] != tar.TypeReg {
			return false
		}
		hdr.Linkname = target
	default:
		return false
	}
	f.accepted[name] = hdr.Typeflag
	hdr.Name = name
	if hdr.Typeflag == tar.TypeDir {
		hdr.Name += "/"
	}
	hdr.Mode &^= 0o6000 // setuid, setgid
	hdr.PAXRecords = nil
	hdr.Xattrs = nil //nolint:staticcheck // deprecated, but still written if left set
	hdr.Format = tar.FormatUnknown
	return true
}

// underStatePaths reports whether an archive entry (absolute path without its
// leading slash, already cleaned) is one of the state paths or inside one.
func underStatePaths(name string, paths []string) bool {
	abs := "/" + name
	for _, p := range paths {
		if abs == p || strings.HasPrefix(abs, p+"/") {
			return true
		}
	}
	return false
}

// Drain refuses every new placement from now on (under e.mu, so no create
// can slip past it) and returns how many containers still exist. Idempotent.
func (e *Engine) Drain() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.draining = true
	return e.count
}

// HostStats is what the controller places sandboxes and removes hosts by.
type HostStats struct {
	Draining  bool             `json:"draining"`
	Slots     int              `json:"slots"`
	Count     int              `json:"count"`
	Creating  int              `json:"creating"`
	Archiving int              `json:"archiving"`
	Sandboxes []SandboxSummary `json:"sandboxes"`
}

// SandboxSummary is one sandbox in HostStats. Times never set are omitted.
type SandboxSummary struct {
	ID           string                `json:"id"`
	Status       runtime.SandboxStatus `json:"status"`
	Labels       map[string]string     `json:"labels,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	LeasedUntil  time.Time             `json:"leased_until,omitzero"`
	LastLeasedAt time.Time             `json:"last_leased_at,omitzero"`
	LastActivity time.Time             `json:"last_activity,omitzero"`
	// Stuck: an archive that cannot succeed. The controller DELETEs it to drain
	// the host (the unsaved state is lost; it is over max_bytes anyway).
	Stuck bool `json:"stuck,omitempty"`
}

// Summary returns the sandbox as the controller sees it.
func (s *Sandbox) Summary() SandboxSummary {
	a := s.Activity()
	return SandboxSummary{
		ID:           s.ID,
		Status:       s.GetStatus(),
		Labels:       s.Config.Labels,
		CreatedAt:    s.CreatedAt,
		LeasedUntil:  a.LeasedUntil,
		LastLeasedAt: a.LastLeasedAt,
		LastActivity: a.LastActivity,
		Stuck:        s.isStuck(),
	}
}

func (s *Sandbox) isStuck() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stuck
}

// HostStats returns the admission counters and every sandbox's summary.
func (e *Engine) HostStats() HostStats {
	e.mu.Lock()
	hs := HostStats{
		Draining:  e.draining,
		Slots:     e.config.MaxSandboxes,
		Count:     e.count,
		Creating:  e.creating,
		Sandboxes: []SandboxSummary{},
	}
	e.mu.Unlock()
	for _, sb := range e.ListSandboxes() {
		sum := sb.Summary()
		if sum.Status == runtime.StatusArchiving {
			hs.Archiving++
		}
		hs.Sandboxes = append(hs.Sandboxes, sum)
	}
	return hs
}

// capReader is io.LimitReader that says so: past the cap it fails with
// errStateTooLarge instead of ending the stream, which would read as a
// truncated archive.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errStateTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errStateTooLarge
	}
	return n, err
}

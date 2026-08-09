// Package logtail follows a log file the way `tail -f` does, in Go, and hands
// completed lines to a callback.
//
// It exists so the desktop's Logs window can stream the app's own log file. The
// file is deliberately the source of truth rather than a tee off the logger:
// tailing needs no ring buffer, no drop policy, and no coupling to zerolog, and
// it yields history from before the window was opened — including a previous
// run's failed startup — for free.
package logtail

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"
)

// Defaults applied when Options leaves a field at zero.
const (
	defaultBacklogBytes = 64 << 10
	defaultPoll         = 250 * time.Millisecond
)

// Options tunes a Tailer.
type Options struct {
	// BacklogBytes is how far back to seek on open for initial history. The
	// seek lands mid-line, so the remainder of that line is discarded and the
	// first emitted line is whole — unless a single line exceeds
	// BacklogBytes, in which case no newline is found before EOF and the
	// first "line" emitted is only the fragment written after open.
	BacklogBytes int64
	// Poll is the interval between checks for new data and for rotation.
	Poll time.Duration
}

func (o Options) withDefaults() Options {
	if o.BacklogBytes <= 0 {
		o.BacklogBytes = defaultBacklogBytes
	}
	if o.Poll <= 0 {
		o.Poll = defaultPoll
	}
	return o
}

// Tailer follows one file path across rotations.
type Tailer struct {
	path string
	opts Options
}

// New returns a Tailer for path. It does not open anything until Run.
func New(path string, opts Options) *Tailer {
	return &Tailer{path: path, opts: opts.withDefaults()}
}

// Run follows the file until ctx is done, calling emit with each batch of
// newly completed lines. emit is never called with an empty slice, and is
// called from Run's goroutine — it must not block for long.
//
// A missing file is not an error: the window can be opened before anything has
// been logged, and a log that has not appeared yet will appear.
func (t *Tailer) Run(ctx context.Context, emit func(lines []string)) {
	var (
		f       *os.File
		fi      os.FileInfo
		partial []byte
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	ticker := time.NewTicker(t.opts.Poll)
	defer ticker.Stop()

	for {
		if f == nil {
			f, fi = t.open()
			partial = nil
		}
		if f != nil {
			// Drain whatever is there, then decide whether the file we are
			// holding is still the file at our path.
			partial = drain(f, partial, emit)
			if t.rotated(fi) {
				// Read to EOF once more before letting go: lumberjack renames
				// the live file, so anything written between the last drain and
				// the rename is only reachable through this handle.
				partial = drain(f, partial, emit)
				flush(&partial, emit)
				f.Close()
				f = nil
				continue
			}
			if truncated(f) {
				f.Close()
				f = nil
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// open opens the path and seeks back at most BacklogBytes, discarding the
// partial line the seek lands in. Returns (nil, nil) when the file is absent.
func (t *Tailer) open() (*os.File, os.FileInfo) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, nil
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil
	}
	if fi.Size() > t.opts.BacklogBytes {
		if _, serr := f.Seek(fi.Size()-t.opts.BacklogBytes, io.SeekStart); serr != nil {
			f.Close()
			return nil, nil
		}
		// The seek landed mid-line. Drop the remainder so the first line the
		// window shows is whole rather than a fragment.
		discardThroughNewline(f)
	}
	return f, fi
}

// discardThroughNewline advances f past the next newline, leaving the offset at
// the start of a complete line.
func discardThroughNewline(f *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
				// Rewind to just after the newline we consumed past.
				if _, serr := f.Seek(int64(i-n+1), io.SeekCurrent); serr != nil {
					return
				}
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// drain reads everything available, prepends any carried-over partial line,
// emits the complete lines, and returns the new partial remainder.
func drain(f *os.File, partial []byte, emit func([]string)) []byte {
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			partial = append(partial, buf[:n]...)
		}
		if err != nil || n == 0 {
			break
		}
	}

	var lines []string
	for {
		i := bytes.IndexByte(partial, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(partial[:i], "\r"))
		// Blank lines are dropped rather than emitted as empty strings. This
		// is a deliberate choice, not an oversight: a log file with genuinely
		// blank lines is not expected here, and dropping them costs nothing
		// the Logs window would otherwise show. It does mean an off-by-one in
		// the backlog seek that lands exactly on a newline (instead of just
		// past it) is unobservable from the emitted lines alone: it produces
		// one leading empty match that this filter silently absorbs, making
		// the output identical to a seek that landed correctly. Anyone
		// tightening the seek arithmetic's tests should know that content
		// assertions alone cannot catch that particular class of bug.
		if line != "" {
			lines = append(lines, line)
		}
		partial = partial[i+1:]
	}
	if len(lines) > 0 {
		emit(lines)
	}
	// Copy the remainder into its own right-sized array. partial here is a
	// reslice of the (possibly large) array built up by the append loop
	// above, so without this copy a short trailing partial line would keep
	// that entire backing array alive across every future call.
	return append([]byte(nil), partial...)
}

// flush emits a trailing unterminated line, used only when abandoning a
// rotated-away handle that will never receive its newline.
func flush(partial *[]byte, emit func([]string)) {
	if len(*partial) > 0 {
		emit([]string{string(*partial)})
		*partial = nil
	}
}

// rotated reports whether the file now at the path is a different file from the
// one described by fi. os.SameFile compares the underlying inode portably, so
// this needs no platform-specific code.
func (t *Tailer) rotated(fi os.FileInfo) bool {
	if fi == nil {
		return false
	}
	current, err := os.Stat(t.path)
	if err != nil {
		// The path is momentarily gone mid-rotation. Keep the handle; the next
		// tick will find the replacement.
		return false
	}
	return !os.SameFile(fi, current)
}

// truncated reports whether the file shrank below our read offset, which means
// it was emptied in place rather than rotated aside.
func truncated(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	return fi.Size() < pos
}

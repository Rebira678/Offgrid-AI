package blobstore

import (
	"errors"
	"fmt"
	"syscall"
)

// DefaultReserve is the free space a blob may never eat into: the database, its WAL, the OS and a
// phone's own needs live on the same disk. A blob is accepted only if it fits and leaves this much.
const DefaultReserve = 64 << 20

// ErrNoSpace is wrapped by every refusal for lack of disk space; errors.As finds the *NoSpaceError
// that says by how much.
var ErrNoSpace = errors.New("blobstore: not enough free disk space")

// NoSpaceError says why a blob was refused, in numbers a person can act on.
type NoSpaceError struct {
	Dir     string
	Need    int64 // bytes the blob still needs
	Free    int64 // bytes free on the disk
	Reserve int64 // bytes that must stay free
}

func (e *NoSpaceError) Error() string {
	return fmt.Sprintf("blobstore: not enough free disk space on %s: the file needs %s, %s is free, and %s is kept in reserve",
		e.Dir, human(e.Need), human(e.Free), human(e.Reserve))
}

func (e *NoSpaceError) Is(target error) bool { return target == ErrNoSpace }

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// FreeFunc reports the bytes available to this user on the disk holding dir.
type FreeFunc func(dir string) (uint64, error)

// SetFreeFunc replaces the free-space probe (tests, or a platform without statfs).
func (s *Store) SetFreeFunc(f FreeFunc) { s.mu.Lock(); s.free = f; s.mu.Unlock() }

// SetReserve sets the space that must stay free (negative: DefaultReserve).
func (s *Store) SetReserve(n int64) {
	s.mu.Lock()
	if n < 0 {
		n = DefaultReserve
	}
	s.reserve = n
	s.mu.Unlock()
}

// Reserve reports the space kept in reserve.
func (s *Store) Reserve() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserve
}

// Free reports the free bytes on the blob disk, and false if this platform cannot tell.
func (s *Store) Free() (uint64, bool) {
	s.mu.Lock()
	f := s.free
	s.mu.Unlock()
	if f == nil {
		f = freeBytes
	}
	n, err := f(s.dir)
	return n, err == nil
}

// CheckSpace returns a *NoSpaceError unless need more bytes fit on the disk with the reserve left. Where the
// free space cannot be measured it allows the write: the filesystem's own ENOSPC is still caught (wrapNoSpace).
func (s *Store) CheckSpace(need int64) error {
	free, ok := s.Free()
	if !ok {
		return nil
	}
	s.mu.Lock()
	reserve := s.reserve
	s.mu.Unlock()
	if need < 0 {
		need = 0
	}
	if free < uint64(need)+uint64(reserve) {
		return &NoSpaceError{Dir: s.dir, Need: need, Free: int64(free), Reserve: reserve}
	}
	return nil
}

// wrapNoSpace turns the filesystem's "no space left on device" into ErrNoSpace.
func wrapNoSpace(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: the disk filled while writing (%v)", ErrNoSpace, err)
	}
	return err
}

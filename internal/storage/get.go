package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Open downloads a file from a provider holding it, checks the bytes against
// the file's CID and returns its content, decrypted when it was stored
// encrypted. The caller must close the reader.
func (s *Store) Open(ctx context.Context, id string) (*File, io.ReadCloser, error) {
	f, err := s.File(id)
	if err != nil {
		return nil, nil, err
	}
	tmp, err := s.fetch(ctx, f, nil)
	if err != nil {
		return nil, nil, err
	}
	if !f.Encrypted {
		return f, tempReader{tmp}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := Unseal(pw, bufio.NewReaderSize(tmp, 1<<20), s.w)
		pw.CloseWithError(err)
		drop(tmp)
	}()
	return f, pr, nil
}

// tempReader removes its file when closed.
type tempReader struct{ *os.File }

func (t tempReader) Close() error {
	drop(t.File)
	return nil
}

// fetch downloads a file's stored bytes from the first copy that returns
// them intact (skipping providers in skip) into a temporary file positioned
// at its start.
func (s *Store) fetch(ctx context.Context, f *File, skip map[common.Address]bool) (*os.File, error) {
	root, err := ipfs.Parse(f.CID)
	if err != nil {
		return nil, err
	}
	var errs []string
	for _, c := range f.Copies {
		if skip[c.Provider] {
			continue
		}
		tmp, err := s.download(ctx, c.peer(), f.CID, f.StoredBytes)
		if err == nil {
			if err = ipfs.Verify(bufio.NewReaderSize(tmp, 1<<20), root); err != nil {
				err = fmt.Errorf("content does not match the file's cid: %w", err)
			} else {
				_, err = tmp.Seek(0, io.SeekStart)
			}
			if err != nil {
				drop(tmp)
			}
		}
		if err == nil {
			return tmp, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", short(c.Provider.Hex()), err))
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 {
		return nil, errors.New("no provider holds this file")
	}
	return nil, fmt.Errorf("could not retrieve the file: %s", strings.Join(errs, "; "))
}

// download reads size stored bytes from a provider into a temporary file.
func (s *Store) download(ctx context.Context, p peer, cid string, size uint64) (*os.File, error) {
	tmp, err := s.tempFile()
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(tmp, 1<<20)
	for off := uint64(0); off < size; {
		data, err := s.readRange(ctx, p, cid, off, min(maxReadBytes, size-off))
		if err == nil {
			_, err = w.Write(data)
		}
		if err != nil {
			drop(tmp)
			return nil, err
		}
		off += uint64(len(data))
	}
	if err := w.Flush(); err != nil {
		drop(tmp)
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		drop(tmp)
		return nil, err
	}
	return tmp, nil
}

// Delete removes a file from the manifest and from every provider holding
// it. Copies a provider cannot be reached for lapse with its lease.
func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	f := s.files[id]
	if f == nil {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.files, id)
	s.dirty = true
	// Another record of the same content keeps the provider's copy.
	shared := map[common.Address]bool{}
	for _, o := range s.files {
		if o.CID == f.CID {
			for _, c := range o.Copies {
				shared[c.Provider] = true
			}
		}
	}
	copies := f.clone().Copies
	s.mu.Unlock()
	s.save()
	for _, c := range copies {
		if shared[c.Provider] {
			continue
		}
		if err := s.deleteCopy(ctx, c.peer(), f.CID); err != nil {
			s.log.Warn("provider could not delete the file yet; will retry", "file", id, "provider", c.Provider.Hex(), "err", err)
			s.mu.Lock()
			s.orphans = append(s.orphans, Orphan{CID: f.CID, Provider: c.Provider, Endpoint: c.Endpoint})
			s.dirty = true
			s.mu.Unlock()
		}
	}
	s.save()
	s.log.Info("file deleted", "file", id, "cid", f.CID)
	s.bus.Emit(events.Info, "storage", fmt.Sprintf("deleted %s", displayName(f)))
	return nil
}

// deleteCopy asks a provider to drop our copy of cid; a copy it no longer
// has counts as deleted.
func (s *Store) deleteCopy(ctx context.Context, p peer, cid string) error {
	var out morrpc.StorageDeleted
	err := s.call(ctx, p, morrpc.MethodStorageDelete, morrpc.StorageReq{CID: cid}, &out)
	if err != nil && strings.Contains(err.Error(), "no such file") {
		return nil
	}
	return err
}

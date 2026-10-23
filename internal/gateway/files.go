package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/storage"
)

// maxUploadBytes bounds one Files API upload (providers set their own limit).
const maxUploadBytes = 16 << 30

// fileObject is the OpenAI Files API object, plus where the file lives.
type fileObject struct {
	ID          string   `json:"id"`
	Object      string   `json:"object"`
	Bytes       int64    `json:"bytes"`
	CreatedAt   int64    `json:"created_at"`
	Filename    string   `json:"filename"`
	Purpose     string   `json:"purpose"`
	Status      string   `json:"status"`
	ContentType string   `json:"content_type,omitempty"`
	CID         string   `json:"cid"`
	Encrypted   bool     `json:"encrypted"`
	StoredBytes uint64   `json:"stored_bytes"`
	Copies      int      `json:"copies"`
	Providers   []string `json:"providers"`
}

func toFileObject(f *storage.File) fileObject {
	o := fileObject{
		ID: f.ID, Object: "file", Bytes: f.Size, CreatedAt: f.Created.Unix(), Filename: f.Name, Purpose: f.Purpose,
		Status: "processed", ContentType: f.ContentType, CID: f.CID, Encrypted: f.Encrypted, StoredBytes: f.StoredBytes,
		Copies: len(f.Copies), Providers: []string{},
	}
	if o.Purpose == "" {
		o.Purpose = "user_data"
	}
	for _, c := range f.Copies {
		o.Providers = append(o.Providers, c.Provider.Hex())
	}
	return o
}

func (s *Server) files(w http.ResponseWriter) (*storage.Store, bool) {
	if s.d.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "storage_disabled", "marketplace storage needs the consumer role")
		return nil, false
	}
	return s.d.Files, true
}

// owns reports whether the caller may see a file: the operator sees all,
// an API key the files it stored.
func owns(p principal, f *storage.File) bool { return p.admin || f.Owner == p.key.ID }

// admitFiles applies the key's request-rate and concurrency limits to a
// Files API transfer; release frees the slot.
func (s *Server) admitFiles(w http.ResponseWriter, p principal) (func(), bool) {
	if p.admin {
		return func() {}, true
	}
	release, err := s.d.Keys.Admit(p.key.ID)
	if err != nil {
		writeLimitError(w, err)
		return nil, false
	}
	return release, true
}

// uploadFile stores a multipart upload ("file", "purpose") on the marketplace.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	st, ok := s.files(w)
	if !ok {
		return
	}
	release, ok := s.admitFiles(w, principalFrom(r))
	if !ok {
		return
	}
	defer release()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_upload", "send multipart/form-data with a \"file\" field")
		return
	}
	p := principalFrom(r)
	var stored *storage.File
	purpose := ""
	public := r.URL.Query().Get("public") == "true"
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_upload", err.Error())
			return
		}
		switch part.FormName() {
		case "purpose":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			purpose = strings.TrimSpace(string(b))
		case "public":
			b, _ := io.ReadAll(io.LimitReader(part, 16))
			public = strings.TrimSpace(string(b)) == "true"
		case "file":
			if stored != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_upload", "one file per upload")
				return
			}
			name := filepath.Base(part.FileName())
			if name == "." || name == string(filepath.Separator) {
				name = ""
			}
			ctype := part.Header.Get("Content-Type")
			if ctype == "" || ctype == "application/octet-stream" {
				if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
					ctype = t
				}
			}
			stored, err = st.Put(context.WithoutCancel(r.Context()), part, storage.PutOptions{
				Name: name, ContentType: ctype, Purpose: purpose, Owner: p.key.ID, Public: public,
			})
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large", fmt.Sprintf("uploads are limited to %d bytes", mbe.Limit))
					return
				}
				writeError(w, http.StatusBadGateway, "api_error", "storage_failed", err.Error())
				return
			}
		}
		part.Close()
	}
	if stored == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_file", "the upload has no \"file\" field")
		return
	}
	if purpose != "" && stored.Purpose != purpose {
		st.SetPurpose(stored.ID, purpose)
		stored.Purpose = purpose
	}
	writeJSON(w, http.StatusOK, toFileObject(stored))
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	st, ok := s.files(w)
	if !ok {
		return
	}
	p := principalFrom(r)
	purpose := r.URL.Query().Get("purpose")
	data := []fileObject{}
	for _, f := range st.Files() {
		if owns(p, f) && (purpose == "" || f.Purpose == purpose) {
			data = append(data, toFileObject(f))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
}

// file resolves the path's file id for the caller.
func (s *Server) file(w http.ResponseWriter, r *http.Request) (*storage.Store, *storage.File, bool) {
	st, ok := s.files(w)
	if !ok {
		return nil, nil, false
	}
	f, err := st.File(r.PathValue("id"))
	if err != nil || !owns(principalFrom(r), f) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "file_not_found", fmt.Sprintf("no file %q", r.PathValue("id")))
		return nil, nil, false
	}
	return st, f, true
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	if _, f, ok := s.file(w, r); ok {
		writeJSON(w, http.StatusOK, toFileObject(f))
	}
}

// fileContent downloads a file from a provider, verifies it against its CID
// and streams it (decrypted) to the client.
func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	st, f, ok := s.file(w, r)
	if !ok {
		return
	}
	release, ok := s.admitFiles(w, principalFrom(r))
	if !ok {
		return
	}
	defer release()
	_, rc, err := st.Open(r.Context(), f.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "storage_unavailable", err.Error())
		return
	}
	defer rc.Close()
	ctype := f.ContentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", fmt.Sprint(f.Size))
	if f.Name != "" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	}
	h.Set("X-Content-CID", f.CID)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		s.d.Log.Warn("file download interrupted", "file", f.ID, "err", err)
	}
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	st, f, ok := s.file(w, r)
	if !ok {
		return
	}
	if err := st.Delete(context.WithoutCancel(r.Context()), f.ID); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "storage_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": f.ID, "object": "file", "deleted": true})
}

// StorageView is the storage status the CLI shows.
type StorageView struct {
	Stats  storage.StoreStats `json:"stats"`
	Leases []storage.Lease    `json:"leases"`
}

func (s *Server) storageStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.files(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, StorageView{Stats: st.Stats(), Leases: nonNilLeases(st.Leases())})
}

func nonNilLeases(l []storage.Lease) []storage.Lease {
	if l == nil {
		return []storage.Lease{}
	}
	return l
}

func (s *Server) storageOffers(w http.ResponseWriter, r *http.Request) {
	st, ok := s.files(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	offers, err := st.Offers(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "marketplace_unavailable", err.Error())
		return
	}
	if offers == nil {
		offers = []storage.Offer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"offers": offers})
}

// storageAudit challenges providers to prove they hold the caller's files.
func (s *Server) storageAudit(w http.ResponseWriter, r *http.Request) {
	st, ok := s.files(w)
	if !ok {
		return
	}
	p := principalFrom(r)
	var ids []string
	if id := r.URL.Query().Get("file"); id != "" {
		ids = []string{id}
	} else if !p.admin {
		for _, f := range st.Files() {
			if owns(p, f) {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"results": []storage.AuditResult{}})
			return
		}
	}
	for _, id := range ids {
		if f, err := st.File(id); err != nil || !owns(p, f) {
			writeError(w, http.StatusNotFound, "invalid_request_error", "file_not_found", fmt.Sprintf("no file %q", id))
			return
		}
	}
	results, err := st.Audit(r.Context(), ids...)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "audit_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// storageRecover rebuilds the manifest from the providers holding the
// wallet's files.
func (s *Server) storageRecover(w http.ResponseWriter, r *http.Request) {
	st, ok := s.files(w)
	if !ok {
		return
	}
	n, err := st.Recover(context.WithoutCancel(r.Context()))
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "recover_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recovered": n})
}

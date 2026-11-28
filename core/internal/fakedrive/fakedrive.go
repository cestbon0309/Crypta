// Package fakedrive is an in-memory imitation of the parts of the Google
// Drive v3 API that Crypta uses, for tests.
package fakedrive

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type file struct {
	ID, Name, Mime, Parent string
	Data                   []byte
	Mod                    time.Time
	Trashed                bool
}

type session struct {
	fileID, name, parent string
	size                 int64
	data                 []byte
	mod                  time.Time
}

type Server struct {
	*httptest.Server
	mu       sync.Mutex
	files    map[string]*file
	sessions map[string]*session
	next     int
	// FailAfter makes the server drop upload chunks once this many bytes
	// were accepted in total (0 = never), keeping only half of that chunk.
	FailAfter int64
	accepted  int64
	Requests  int
	served    int64
	streams   int
	// Streams is the most downloads that were running at one time.
	Streams int
}

func New() *Server {
	s := &Server{files: map[string]*file{}, sessions: map[string]*session{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Accepted is the number of upload bytes stored so far.
func (s *Server) Accepted() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// Served is the number of download bytes sent so far.
func (s *Server) Served() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

func (s *Server) id() string { s.next++; return fmt.Sprintf("id%d", s.next) }

func (s *Server) meta(f *file) map[string]any {
	m := map[string]any{"id": f.ID, "name": f.Name, "mimeType": f.Mime, "modifiedTime": f.Mod.UTC().Format(time.RFC3339Nano)}
	if f.Mime != "application/vnd.google-apps.folder" {
		m["size"] = strconv.Itoa(len(f.Data))
	}
	return m
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests++
	if r.URL.Path == "/token" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer at" {
		http.Error(w, "unauthorized", 401)
		return
	}
	q := r.URL.Query()
	switch {
	case strings.HasPrefix(r.URL.Path, "/session/"):
		s.upload(w, r)
	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files"):
		var m struct {
			Name         string
			Parents      []string
			ModifiedTime time.Time
		}
		json.NewDecoder(r.Body).Decode(&m)
		size, _ := strconv.ParseInt(r.Header.Get("X-Upload-Content-Length"), 10, 64)
		ss := &session{name: m.Name, size: size, mod: m.ModifiedTime}
		if len(m.Parents) > 0 {
			ss.parent = m.Parents[0]
		}
		if r.Method == "PATCH" {
			ss.fileID = strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
		}
		id := s.id()
		s.sessions[id] = ss
		w.Header().Set("Location", s.URL+"/session/"+id)
	case r.URL.Path == "/drive/v3/files" && r.Method == "GET":
		parent := strings.Split(q.Get("q"), "'")[1]
		var out []map[string]any
		for _, f := range s.files {
			if f.Parent == parent && !f.Trashed {
				out = append(out, s.meta(f))
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": out})
	case r.URL.Path == "/drive/v3/files" && r.Method == "POST":
		var m struct {
			Name, MimeType string
			Parents        []string
		}
		json.NewDecoder(r.Body).Decode(&m)
		f := &file{ID: s.id(), Name: m.Name, Mime: m.MimeType, Parent: m.Parents[0], Mod: time.Now()}
		s.files[f.ID] = f
		json.NewEncoder(w).Encode(s.meta(f))
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
		f := s.files[strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")]
		if f == nil || f.Trashed {
			http.Error(w, "not found", 404)
			return
		}
		if r.Method == "PATCH" {
			var m struct {
				Name    string
				Trashed bool
			}
			json.NewDecoder(r.Body).Decode(&m)
			if m.Name != "" {
				f.Name = m.Name
			}
			if p := q.Get("addParents"); p != "" {
				f.Parent = p
			}
			f.Trashed = m.Trashed
			json.NewEncoder(w).Encode(s.meta(f))
			return
		}
		data := f.Data
		if rng := r.Header.Get("Range"); rng != "" {
			a, b, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
			start, _ := strconv.Atoi(a)
			end := len(data) - 1
			if b != "" {
				end, _ = strconv.Atoi(b)
			}
			if end >= len(data) {
				end = len(data) - 1
			}
			if start > end {
				http.Error(w, "bad range", 416)
				return
			}
			data = data[start : end+1]
			w.WriteHeader(206)
		}
		// Streams run side by side, as on Drive.
		s.streams++
		s.Streams = max(s.Streams, s.streams)
		s.mu.Unlock()
		n, _ := w.Write(data)
		s.mu.Lock()
		s.streams--
		s.served += int64(n)
	default:
		http.Error(w, "unknown", 400)
	}
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	ss := s.sessions[strings.TrimPrefix(r.URL.Path, "/session/")]
	if ss == nil {
		http.Error(w, "gone", 404)
		return
	}
	cr := strings.TrimPrefix(r.Header.Get("Content-Range"), "bytes ")
	rng, _, _ := strings.Cut(cr, "/")
	body, _ := io.ReadAll(r.Body)
	if rng != "*" {
		a, _, _ := strings.Cut(rng, "-")
		start, _ := strconv.ParseInt(a, 10, 64)
		if start != int64(len(ss.data)) {
			http.Error(w, "wrong offset", 400)
			return
		}
		if int64(len(ss.data)+len(body)) < ss.size && len(body)%(256*1024) != 0 {
			http.Error(w, "chunk not aligned", 400)
			return
		}
		if s.FailAfter > 0 && s.accepted+int64(len(body)) > s.FailAfter {
			s.FailAfter = 0
			half := len(body) / 2 / (256 * 1024) * (256 * 1024)
			s.accepted += int64(half)
			ss.data = append(ss.data, body[:half]...)
			http.Error(w, "simulated failure", 400)
			return
		}
		s.accepted += int64(len(body))
		ss.data = append(ss.data, body...)
	}
	if int64(len(ss.data)) < ss.size {
		if len(ss.data) > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(ss.data)-1))
		}
		w.WriteHeader(308)
		return
	}
	f := s.files[ss.fileID]
	if f == nil {
		f = &file{ID: s.id(), Name: ss.name, Parent: ss.parent, Mime: "application/octet-stream"}
		s.files[f.ID] = f
	}
	f.Data, f.Mod = ss.data, ss.mod
	json.NewEncoder(w).Encode(s.meta(f))
}

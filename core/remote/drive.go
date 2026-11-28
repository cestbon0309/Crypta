package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	folderMime      = "application/vnd.google-apps.folder"
	defaultAPIBase  = "https://www.googleapis.com"
	defaultTokenURL = "https://oauth2.googleapis.com/token"
	listFields      = "nextPageToken,files(id,name,mimeType,size,modifiedTime)"
)

type DriveConfig struct {
	ClientID     string          `json:"clientId"`
	ClientSecret string          `json:"clientSecret,omitempty"`
	Token        json.RawMessage `json:"token"` // oauth2 token JSON, as stored in rclone.conf
	RootFolderID string          `json:"rootFolderId,omitempty"`
	TeamDrive    string          `json:"teamDrive,omitempty"`
	// Overrides for tests.
	APIBase  string `json:"apiBase,omitempty"`
	TokenURL string `json:"tokenUrl,omitempty"`
}

type Drive struct {
	cfg DriveConfig
	hc  *http.Client
	// bulk speaks HTTP/1.1: one connection per request in flight, where
	// HTTP/2 would put them all on one.
	bulk *http.Client
	base string
}

func NewDrive(cfg DriveConfig) (*Drive, error) {
	var tok oauth2.Token
	if err := json.Unmarshal(cfg.Token, &tok); err != nil {
		return nil, fmt.Errorf("drive: bad token: %w", err)
	}
	if tok.RefreshToken == "" && tok.AccessToken == "" {
		return nil, fmt.Errorf("drive: token has no refresh_token")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("drive: client_id is required (rclone's shared client id is being retired; create your own, see docs/google-setup.md)")
	}
	d := &Drive{cfg: cfg, base: strings.TrimRight(cfg.APIBase, "/")}
	if d.base == "" {
		d.base = defaultAPIBase
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	oc := &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: oauth2.Endpoint{TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams}}
	ts := oauth2.ReuseTokenSource(nil, oc.TokenSource(context.Background(), &tok))
	d.hc = oauth2.NewClient(context.Background(), ts)
	d.bulk = &http.Client{Transport: &oauth2.Transport{Source: ts, Base: http1Transport()}}
	return d, nil
}

// http1Transport never speaks HTTP/2. A clone of the default transport
// still offers h2 to the server, so what is offered is set here as well.
func http1Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	t.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	t.MaxIdleConnsPerHost = 16
	return t
}

func (d *Drive) RootID() string {
	if d.cfg.RootFolderID != "" {
		return d.cfg.RootFolderID
	}
	if d.cfg.TeamDrive != "" {
		return d.cfg.TeamDrive
	}
	return "root"
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("drive: HTTP %d: %s", e.Status, e.Body) }

func retryable(status int, body string) bool {
	return status == 429 || status >= 500 || (status == 403 && strings.Contains(strings.ToLower(body), "ratelimit"))
}

// do sends a request with a replayable body, retrying rate limits and 5xx.
// A response with a status in ok is returned open; anything else is an error.
func (d *Drive) do(ctx context.Context, method, u string, hdr http.Header, body []byte, ok ...int) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			// Wrapped after NewRequest, which has set the length from the bytes.
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(sent(ctx, bytes.NewReader(body))), nil
			}
			req.Body, _ = req.GetBody()
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		hc := d.hc
		if bulk(ctx) {
			hc = d.bulk
		}
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var re *oauth2.RetrieveError
			if errors.As(err, &re) {
				return nil, fmt.Errorf("drive: Google sign-in was rejected (token expired or revoked): %w", err)
			}
			lastErr = err
			continue
		}
		for _, s := range ok {
			if resp.StatusCode == s {
				return resp, nil
			}
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		lastErr = &apiError{resp.StatusCode, string(b)}
		if resp.StatusCode == 404 {
			return nil, ErrNotFound
		}
		if !retryable(resp.StatusCode, string(b)) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (d *Drive) json(ctx context.Context, method, u string, in, out any, ok ...int) error {
	var body []byte
	hdr := http.Header{}
	if in != nil {
		body, _ = json.Marshal(in)
		hdr.Set("Content-Type", "application/json")
	}
	resp, err := d.do(ctx, method, u, hdr, body, ok...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type driveFile struct {
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name,omitempty"`
	MimeType     string   `json:"mimeType,omitempty"`
	Size         string   `json:"size,omitempty"`
	ModifiedTime string   `json:"modifiedTime,omitempty"`
	Parents      []string `json:"parents,omitempty"`
}

func (f driveFile) entry() Entry {
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	mt, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	return Entry{ID: f.ID, Name: f.Name, IsDir: f.MimeType == folderMime, Size: size, ModTime: mt}
}

func (d *Drive) List(ctx context.Context, parentID string) ([]Entry, error) {
	var out []Entry
	pageToken := ""
	for {
		q := url.Values{
			"q":                         {fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(parentID, "'", `\'`))},
			"fields":                    {listFields},
			"pageSize":                  {"1000"},
			"supportsAllDrives":         {"true"},
			"includeItemsFromAllDrives": {"true"},
		}
		if d.cfg.TeamDrive != "" {
			q.Set("corpora", "drive")
			q.Set("driveId", d.cfg.TeamDrive)
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var res struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := d.json(ctx, "GET", d.base+"/drive/v3/files?"+q.Encode(), nil, &res, 200); err != nil {
			return nil, err
		}
		for _, f := range res.Files {
			// Google Docs and shortcuts have no content to decrypt.
			if f.MimeType != folderMime && strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") {
				continue
			}
			out = append(out, f.entry())
		}
		if pageToken = res.NextPageToken; pageToken == "" {
			return out, nil
		}
	}
}

func (d *Drive) Mkdir(ctx context.Context, parentID, name string) (Entry, error) {
	var f driveFile
	err := d.json(ctx, "POST", d.base+"/drive/v3/files?supportsAllDrives=true&fields=id,name,mimeType,modifiedTime",
		driveFile{Name: name, MimeType: folderMime, Parents: []string{parentID}}, &f, 200)
	return f.entry(), err
}

func (d *Drive) Move(ctx context.Context, id, oldParentID, newParentID, newName string) error {
	q := url.Values{"supportsAllDrives": {"true"}, "fields": {"id"}}
	if oldParentID != newParentID {
		q.Set("addParents", newParentID)
		q.Set("removeParents", oldParentID)
	}
	return d.json(ctx, "PATCH", d.base+"/drive/v3/files/"+url.PathEscape(id)+"?"+q.Encode(), driveFile{Name: newName}, nil, 200)
}

// Delete moves the file to the Drive trash, like rclone does by default.
func (d *Drive) Delete(ctx context.Context, id string) error {
	return d.json(ctx, "PATCH", d.base+"/drive/v3/files/"+url.PathEscape(id)+"?supportsAllDrives=true&fields=id", map[string]bool{"trashed": true}, nil, 200)
}

func (d *Drive) Open(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	hdr := http.Header{}
	if offset > 0 || length >= 0 {
		rng := fmt.Sprintf("bytes=%d-", offset)
		if length >= 0 {
			rng += strconv.FormatInt(offset+length-1, 10)
		}
		hdr.Set("Range", rng)
	}
	resp, err := d.do(ctx, "GET", d.base+"/drive/v3/files/"+url.PathEscape(id)+"?alt=media&supportsAllDrives=true&acknowledgeAbuse=true", hdr, nil, 200, 206)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (d *Drive) StartUpload(ctx context.Context, parentID, name, replaceID string, size int64, modTime time.Time) (string, error) {
	meta := driveFile{ModifiedTime: modTime.UTC().Format(time.RFC3339Nano)}
	method, u := "POST", d.base+"/upload/drive/v3/files?uploadType=resumable&supportsAllDrives=true"
	if replaceID != "" {
		method, u = "PATCH", d.base+"/upload/drive/v3/files/"+url.PathEscape(replaceID)+"?uploadType=resumable&supportsAllDrives=true"
	} else {
		meta.Name, meta.Parents = name, []string{parentID}
	}
	body, _ := json.Marshal(meta)
	hdr := http.Header{
		"Content-Type":            {"application/json; charset=UTF-8"},
		"X-Upload-Content-Type":   {"application/octet-stream"},
		"X-Upload-Content-Length": {strconv.FormatInt(size, 10)},
	}
	resp, err := d.do(ctx, method, u, hdr, body, 200)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("drive: no upload session URL returned")
	}
	return loc, nil
}

// parseRange reads the "Range: bytes=0-N" header of a 308 response.
func parseRange(resp *http.Response) int64 {
	r := resp.Header.Get("Range")
	if i := strings.LastIndex(r, "-"); i >= 0 {
		if n, err := strconv.ParseInt(r[i+1:], 10, 64); err == nil {
			return n + 1
		}
	}
	return 0
}

func (d *Drive) put(ctx context.Context, session string, contentRange string, data []byte) (*http.Response, error) {
	resp, err := d.do(ctx, "PUT", session, http.Header{"Content-Range": {contentRange}}, data, 200, 201, 308)
	if err == ErrNotFound {
		return nil, ErrSessionGone
	}
	if ae, ok := err.(*apiError); ok && (ae.Status == 410) {
		return nil, ErrSessionGone
	}
	return resp, err
}

func (d *Drive) UploadStatus(ctx context.Context, session string, size int64) (int64, bool, error) {
	resp, err := d.put(ctx, session, fmt.Sprintf("bytes */%d", size), nil)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 308 {
		return size, true, nil
	}
	return parseRange(resp), false, nil
}

func (d *Drive) UploadChunk(ctx context.Context, session string, offset int64, data []byte, size int64) (bool, error) {
	if len(data) == 0 { // empty file
		resp, err := d.put(ctx, session, fmt.Sprintf("bytes */%d", size), nil)
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		return resp.StatusCode != 308, nil
	}
	resp, err := d.put(ctx, session, fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(data))-1, size), data)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	if end := offset + int64(len(data)); resp.StatusCode == 308 && parseRange(resp) != end {
		return false, fmt.Errorf("Drive stored %d of %d bytes", parseRange(resp), end)
	}
	return resp.StatusCode != 308, nil
}

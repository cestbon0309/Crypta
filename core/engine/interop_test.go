package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
)

// These tests run the real rclone binary against the same directory as the
// engine. Set RCLONE_BIN or put rclone on PATH; otherwise they are skipped.
func rcloneBin(t *testing.T) string {
	bin := os.Getenv("RCLONE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("rclone"); err != nil {
			t.Skip("rclone not found")
		}
	}
	return bin
}

type rig struct {
	t      *testing.T
	bin    string
	conf   string
	store  string
	fs     *FS
	m      *Manager
	files  string
	cancel context.CancelFunc
}

func newRig(t *testing.T, extra string, opt crypt.Options) *rig {
	r := &rig{t: t, bin: rcloneBin(t)}
	dir := t.TempDir()
	r.store, r.files, r.conf = filepath.Join(dir, "store"), filepath.Join(dir, "files"), filepath.Join(dir, "rclone.conf")
	os.MkdirAll(r.files, 0o755)
	obscure := func(s string) string {
		out, err := exec.Command(r.bin, "obscure", s).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	conf := "[sec]\ntype = crypt\nremote = " + r.store + "\npassword = " + obscure(opt.Password) + "\n"
	if opt.Salt != "" {
		conf += "password2 = " + obscure(opt.Salt) + "\n"
	}
	conf += extra
	os.WriteFile(r.conf, []byte(conf), 0o600)
	if got, err := crypt.Reveal(obscure("pässword 1")); err != nil || got != "pässword 1" {
		t.Fatalf("Reveal: %q %v", got, err)
	}
	var err error
	if r.fs, err = New(&Config{Backend: "local", LocalRoot: r.store, Crypt: opt}); err != nil {
		t.Fatal(err)
	}
	if r.m, err = NewManager(filepath.Join(dir, "state"), r.files); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	r.m.SetFS(r.fs)
	go r.m.Run(ctx)
	return r
}

func (r *rig) rclone(args ...string) string {
	out, err := exec.Command(r.bin, append([]string{"--config", r.conf}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("rclone %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *rig) wait(id string) Transfer {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, t := range r.m.List() {
			if t.ID == id && (t.State == StateDone || t.State == StateFailed) {
				if t.State == StateFailed {
					r.t.Fatalf("transfer failed: %s", t.Error)
				}
				return t
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatal("transfer timed out")
	return Transfer{}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

var sizes = []int{0, 1, 65535, 65536, 65537, 3*65536 + 123, 20*1024*1024 + 7}

func testInterop(t *testing.T, extra string, opt crypt.Options) {
	r := newRig(t, extra, opt)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	names := []string{"empty", "one", "a.txt", "中文 文件名.mkv", "exact", "three blocks.bin", "big-" + strings.Repeat("x", 120) + ".rmvb"}
	data := map[string][]byte{}
	for i, n := range names {
		p := filepath.Join(src, "dir one", "深 层", n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		data[n] = randBytes(sizes[i])
		os.WriteFile(p, data[n], 0o644)
	}
	// rclone writes, Crypta reads.
	r.rclone("copy", src, "sec:")
	items, err := r.fs.List(ctx, "dir one/深 层", false)
	if err != nil || len(items) != len(names) {
		t.Fatalf("list: %v, %d items", err, len(items))
	}
	for _, it := range items {
		want := data[it.Name]
		if want == nil || it.Size != int64(len(want)) {
			t.Fatalf("%q: size %d, want %d", it.Name, it.Size, len(want))
		}
		rc, err := r.fs.Open(ctx, &it, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%q: content differs (%v)", it.Name, err)
		}
		// Random access, as used when seeking in a video.
		for _, rg := range [][2]int64{{0, 10}, {65530, 20}, {65536, 1}, {100000, 200000}, {int64(len(want)) - 5, 100}, {19 * 1024 * 1024, 70000}} {
			if rg[0] < 0 || rg[0] >= int64(len(want)) {
				continue
			}
			end := min(rg[0]+rg[1], int64(len(want)))
			rc, err := r.fs.Open(ctx, &it, rg[0], rg[1])
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || !bytes.Equal(got, want[rg[0]:end]) {
				t.Fatalf("%q range %v differs (%v)", it.Name, rg, err)
			}
		}
	}
	// Crypta writes, rclone reads.
	for n, b := range data {
		os.WriteFile(filepath.Join(r.files, n), b, 0o644)
		tr, err := r.m.Add(Transfer{Kind: KindUpload, Local: n, Remote: "from crypta/子目录"})
		if err != nil {
			t.Fatal(err)
		}
		r.wait(tr.ID)
	}
	back := filepath.Join(t.TempDir(), "back")
	r.rclone("copy", "sec:from crypta", back)
	r.rclone("cryptcheck", filepath.Join(r.files), "sec:from crypta/子目录")
	for n, b := range data {
		got, err := os.ReadFile(filepath.Join(back, "子目录", n))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("rclone could not read %q back: %v", n, err)
		}
	}
	// Download through the queue.
	tr, _ := r.m.Add(Transfer{Kind: KindDownload, Remote: "from crypta/子目录/three blocks.bin", Local: "dl/out.bin"})
	r.wait(tr.ID)
	if got, _ := os.ReadFile(filepath.Join(r.files, "dl/out.bin")); !bytes.Equal(got, data["three blocks.bin"]) {
		t.Fatal("download differs")
	}
	// Rename, move, delete; rclone must agree.
	if err := r.fs.Move(ctx, "from crypta/子目录/a.txt", "moved/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := r.fs.Delete(ctx, "from crypta/子目录/one"); err != nil {
		t.Fatal(err)
	}
	ls := r.rclone("lsf", "-R", "sec:")
	if !strings.Contains(ls, "moved/b.txt") || strings.Contains(ls, "子目录/a.txt") || strings.Contains(ls, "子目录/one\n") {
		t.Fatalf("unexpected listing:\n%s", ls)
	}
}

func TestInteropStandard(t *testing.T) {
	testInterop(t, "", crypt.Options{Password: "correct horse"})
}

func TestInteropSaltBase64(t *testing.T) {
	testInterop(t, "filename_encoding = base64\n", crypt.Options{Password: "pw", Salt: "salty", FilenameEncoding: "base64"})
}

func TestInteropNoDirNames(t *testing.T) {
	testInterop(t, "directory_name_encryption = false\n", crypt.Options{Password: "pw", PlainDirNames: true, FilenameEncryption: "standard"})
}

func TestInteropOff(t *testing.T) {
	testInterop(t, "filename_encryption = off\n", crypt.Options{Password: "pw", FilenameEncryption: "off"})
}

func TestWrongPassword(t *testing.T) {
	r := newRig(t, "", crypt.Options{Password: "right"})
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, []byte("hello"), 0o644)
	r.rclone("copyto", src, "sec:f")
	bad, _ := New(&Config{Backend: "local", LocalRoot: r.store, Crypt: crypt.Options{Password: "wrong"}})
	if items, _ := bad.List(context.Background(), "", false); len(items) != 0 {
		t.Fatalf("wrong password listed %d items", len(items))
	}
}

const chunkerConf = "[big]\ntype = chunker\nremote = sec:\nchunk_size = 1M\n"

// Files above the chunk size are stored as rclone's chunker stores them.
func TestChunkerInterop(t *testing.T) {
	r := newRig(t, chunkerConf, crypt.Options{Password: "pw", Salt: "salt"})
	r.fs.ChunkSize = 1 << 20
	defer func(n int64) { partRequest = n }(partRequest)
	partRequest = 256 * 1024
	ctx := context.Background()

	// Written by the engine, read by rclone.
	mine := randBytes(5<<20 + 4321)
	small := randBytes(1 << 20) // exactly one chunk: stays a plain file
	os.WriteFile(filepath.Join(r.files, "mine.bin"), mine, 0o644)
	os.WriteFile(filepath.Join(r.files, "small.bin"), small, 0o644)
	a, _ := r.m.Add(Transfer{Kind: KindUpload, Local: "mine.bin", Remote: "dir"})
	b, _ := r.m.Add(Transfer{Kind: KindUpload, Local: "small.bin", Remote: "dir"})
	r.wait(a.ID)
	r.wait(b.ID)
	out := filepath.Join(t.TempDir(), "out")
	r.rclone("copy", "big:dir", out)
	for name, want := range map[string][]byte{"mine.bin": mine, "small.bin": small} {
		if got, _ := os.ReadFile(filepath.Join(out, name)); !bytes.Equal(got, want) {
			t.Fatalf("rclone read %s differently (%d bytes)", name, len(got))
		}
	}
	r.rclone("check", "--download", r.files, "big:dir")
	if ls := r.rclone("lsf", "big:dir"); strings.Contains(ls, "rclone_chunk") {
		t.Fatalf("rclone sees loose chunks: %s", ls)
	}
	// The MD5 in the metadata is the one rclone computes.
	r.rclone("check", r.files, "big:dir")

	// Written by rclone, read by the engine.
	theirs := randBytes(3<<20 + 77)
	src := filepath.Join(t.TempDir(), "theirs.bin")
	os.WriteFile(src, theirs, 0o644)
	r.rclone("copyto", src, "big:dir/theirs.bin")
	items, err := r.fs.List(ctx, "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, it := range items {
		sizes[it.Name] = it.Size
	}
	if len(sizes) != 3 || sizes["theirs.bin"] != int64(len(theirs)) || sizes["mine.bin"] != int64(len(mine)) || sizes["small.bin"] != 1<<20 {
		t.Fatalf("listing: %v", sizes)
	}
	read := func(p string, off, n int64) []byte {
		it, err := r.fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := r.fs.Open(ctx, it, off, n)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !bytes.Equal(read("dir/theirs.bin", 0, -1), theirs) {
		t.Fatal("engine read rclone's chunked file differently")
	}
	// Ranges that start inside a chunk and cross chunk borders.
	for _, rg := range [][2]int64{{1<<20 - 5, 10}, {700000, 2 << 20}, {3 << 20, 77}, {3<<20 + 70, 100}} {
		end := min(rg[0]+rg[1], int64(len(theirs)))
		if !bytes.Equal(read("dir/theirs.bin", rg[0], rg[1]), theirs[rg[0]:end]) {
			t.Fatalf("range %v differs", rg)
		}
	}

	// Rename, move and delete take the chunks along.
	if err := r.fs.Move(ctx, "dir/mine.bin", "moved/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	r.rclone("copyto", "big:moved/renamed.bin", filepath.Join(out, "renamed.bin"))
	if got, _ := os.ReadFile(filepath.Join(out, "renamed.bin")); !bytes.Equal(got, mine) {
		t.Fatal("moved file differs")
	}
	if err := r.fs.Delete(ctx, "moved/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	if err := r.fs.Delete(ctx, "dir/theirs.bin"); err != nil {
		t.Fatal(err)
	}
	if ls := strings.TrimSpace(r.rclone("lsf", "-R", "--files-only", "sec:")); ls != "dir/small.bin" {
		t.Fatalf("left behind: %q", ls)
	}
}

func TestChunkerConf(t *testing.T) {
	base := "[d]\ntype = drive\ntoken = {\"access_token\":\"a\",\"refresh_token\":\"r\"}\n[sec]\ntype = crypt\nremote = d:x\npassword = " + "" + "\n"
	ini := func(s string) map[string]iniSection {
		secs, _ := parseINI(s)
		return secs
	}
	if n, err := chunkerOf(ini(base+"[big]\ntype = chunker\nremote = sec:\nchunk_size = 256M\n"), "sec"); err != nil || n != 256<<20 {
		t.Fatal(n, err)
	}
	if n, _ := chunkerOf(ini(base), "sec"); n != 0 {
		t.Fatal(n)
	}
	if _, err := chunkerOf(ini(base+"[big]\ntype = chunker\nremote = sec:\nname_format = *.part##\n"), "sec"); err == nil {
		t.Fatal("custom name_format accepted")
	}
	for in, want := range map[string]int64{"2G": 2 << 30, "100": 100 << 10, "1Mi": 1 << 20, "512B": 512, "1.5M": 3 << 19} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Fatal(in, got, err)
		}
	}
}

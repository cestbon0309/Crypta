package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in for pikpakcli: it "downloads" what $FAKE_PIKPAK holds for the
// path it is given, and like pikpakcli reports failures only in its output.
const fakePikPak = `#!/bin/sh
[ "$1" = --config ] && grep -q '^username: "me"$' "$2" || { echo "Login failed"; exit 0; }
[ -n "$XDG_CONFIG_HOME" ] || { echo "Login failed"; exit 0; }
out=$5; src=$FAKE_PIKPAK$(printf %s "$7" | sed 's/\\\(.\)/\1/g')
echo "$7" >>"$FAKE_PIKPAK.args"
if [ -e "$src.partial" ]; then
	touch "$out/$(basename "$src").pikpakclidownload"
	rm "$src.partial"
	exit 0
fi
[ -e "$src" ] || { echo "Resolve download target failed: $7"; exit 0; }
cp -R "$src" "$out/"
rm -f "$out/$(basename "$src").pikpakclidownload"
`

func TestPikPakJobs(t *testing.T) {
	m, fs, _ := driveRig(t)
	dir := t.TempDir()
	if _, err := m.Add(Transfer{Kind: KindURL, URL: "pikpak:/a.bin"}); err == nil {
		t.Fatal("accepted without a PikPak account")
	}
	// A job saved while PikPak was on, run after it was turned off.
	if err := m.pull(t.Context(), &Transfer{ID: "x", Source: "/a.bin"}); err == nil {
		t.Fatal("ran without a PikPak account")
	}
	bin := filepath.Join(dir, "pikpakcli")
	if err := os.WriteFile(bin, []byte(fakePikPak), 0o755); err != nil {
		t.Fatal(err)
	}
	cloud := filepath.Join(dir, "cloud")
	t.Setenv("FAKE_PIKPAK", cloud)
	os.MkdirAll(filepath.Join(cloud, "show [1080p]", "extras"), 0o755)
	os.WriteFile(filepath.Join(cloud, "show [1080p]", "e1.mkv"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(cloud, "show [1080p]", "extras", "e2.mkv"), []byte("two"), 0o644)
	os.WriteFile(filepath.Join(cloud, "a.bin"), []byte("file"), 0o644)
	var err error
	if m.PikPak, err = NewPikPak(bin, filepath.Join(dir, "pikpak-home"), "me", `p"w: #`, ""); err != nil {
		t.Fatal(err)
	}
	settle := func() {
		t.Helper()
		for _, tr := range m.List() {
			if s := waitState(t, m, tr.ID, StateDone, StateFailed); s.State != StateDone {
				t.Fatalf("%s: %s", s.Name, s.Error)
			}
		}
	}

	// A folder becomes one upload per file, under the same folders.
	tr, err := m.Add(Transfer{Kind: KindURL, URL: "pikpak:show [1080p]", Remote: "dl"})
	if err != nil || tr.Kind != KindPikPak || tr.Name != "show [1080p]" {
		t.Fatal(tr, err)
	}
	waitState(t, m, tr.ID, StateDone, StateFailed)
	settle()
	if string(read(t, fs, "dl/show [1080p]/e1.mkv")) != "one" || string(read(t, fs, "dl/show [1080p]/extras/e2.mkv")) != "two" {
		t.Fatal("folder was not uploaded")
	}
	if args, _ := os.ReadFile(cloud + ".args"); strings.TrimSpace(string(args)) != `/show \[1080p]` {
		t.Fatalf("pikpakcli was given %q", args)
	}
	if left, _ := os.ReadDir(filepath.Join(m.filesDir, "pikpak")); len(left) != 0 {
		t.Fatalf("downloaded files were left behind: %v", left)
	}

	// A download pikpakcli gave up on fails, and a retry finishes it.
	os.WriteFile(filepath.Join(cloud, "a.bin.partial"), nil, 0o644)
	tr, _ = m.Add(Transfer{Kind: KindURL, URL: "pikpak:/a.bin", Remote: "dl"})
	if s := waitState(t, m, tr.ID, StateDone, StateFailed); s.State != StateFailed {
		t.Fatal("an unfinished download passed as done")
	}
	m.Act(tr.ID, "resume")
	waitState(t, m, tr.ID, StateDone)
	settle()
	if string(read(t, fs, "dl/a.bin")) != "file" {
		t.Fatal("file was not uploaded")
	}

	tr, _ = m.Add(Transfer{Kind: KindURL, URL: "pikpak:/missing", Remote: "dl"})
	if s := waitState(t, m, tr.ID, StateDone, StateFailed); s.State != StateFailed || !strings.Contains(s.Error, "Resolve download target failed") {
		t.Fatalf("missing path: %+v", s)
	}
	m.Act(tr.ID, "remove")
	if left, _ := os.ReadDir(filepath.Join(m.filesDir, "pikpak")); len(left) != 0 {
		t.Fatalf("a removed job left its folder: %v", left)
	}
}

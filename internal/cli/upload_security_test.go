package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadRejectsSymlinkedSources(t *testing.T) {
	api, srv := newFakeAPI(t)
	dir := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"index.html": "private"})
	archive := filepath.Join(outside, "bundle.tar.gz")
	if err := os.WriteFile(archive, []byte("private archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(dir, "dist")
	linkedArchive := filepath.Join(dir, "bundle.tar.gz")
	if err := os.Symlink(outside, linkedDir); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	if err := os.Symlink(archive, linkedArchive); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"deploy directory", []string{"deploy", "--flat", "blog", linkedDir}},
		{"draft directory", []string{"draft", "blog", linkedDir}},
		{"deploy archive", []string{"deploy", "--flat", "blog", linkedArchive}},
		{"draft archive", []string{"draft", "blog", linkedArchive}},
		{"trailing separator", []string{"deploy", "--flat", "blog", linkedDir + string(filepath.Separator)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, srv.URL, "", tc.args...)
			if got.code == 0 || !strings.Contains(got.stderr, "symbolic link") {
				t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
			}
		})
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, req := range api.reqs {
		if req.method == "POST" {
			t.Fatalf("symlink source was uploaded to %s", req.path)
		}
	}
}

func TestPackDirRejectsLinkedRoot(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"index.html": "private"})
	link := filepath.Join(t.TempDir(), "dist")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, _, err := packDir(link, 0); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("packDir symlink root: %v", err)
	}
}

func TestPinnedUploadSourcesSurvivePathReplacement(t *testing.T) {
	work := t.TempDir()
	build := filepath.Join(work, "dist")
	writeTree(t, build, map[string]string{"index.html": "expected"})
	outside := filepath.Join(work, "outside")
	writeTree(t, outside, map[string]string{"index.html": "secret"})

	source, err := openUploadSource(build)
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	if err := os.Rename(build, filepath.Join(work, "old-dist")); err != nil {
		t.Skipf("cannot rename an opened directory: %v", err)
	}
	if err := os.Symlink(outside, build); err != nil {
		t.Skipf("cannot replace source with link: %v", err)
	}
	data, stats, err := packOpenedDir(source.dir, build, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := untar(t, data)["index.html"]; got != "expected" || stats.Files != 1 {
		t.Fatalf("pinned build: %q (%+v)", got, stats)
	}

	archive := filepath.Join(work, "bundle.tar.gz")
	if err := os.WriteFile(archive, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := openUploadSource(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := os.Rename(archive, archive+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "index.html"), archive); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(a.file)
	if err != nil || string(contents) != "safe" {
		t.Fatalf("pinned archive: %q, %v", contents, err)
	}
}

func TestDeployDirectArchive(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/versions", 201, `{"version":{"number":1,"files":1,"size":7}}`)
	archive := filepath.Join(t.TempDir(), "site.tar.gz")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := run(t, srv.URL, "", "deploy", "--flat", "blog", archive)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	if body := string(api.last("POST /api/flats/blog/versions").body); body != "archive" {
		t.Fatalf("uploaded %q", body)
	}
}

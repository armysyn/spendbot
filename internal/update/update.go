// Package update finds out whether a newer spendbot is released on GitHub and installs it:
// it downloads the archive for this computer, checks it against SHA256SUMS.txt from the same
// release and puts the new program in place of the running one. A build from source (a version
// that is not a plain release tag) can install a release too: the release program takes the
// place of the built one, and building from source again puts a built one back.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is where releases come from.
const Repo = "armysyn/spendbot"

const (
	maxArchive = 200 << 20
	maxBinary  = 200 << 20
)

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type Release struct {
	Tag       string    `json:"tag_name"`
	Name      string    `json:"name"`
	Body      string    `json:"body"`
	URL       string    `json:"html_url"`
	Published time.Time `json:"published_at"`
	Assets    []Asset   `json:"assets"`
}

// Status is what the settings page shows.
type Status struct {
	Current   string
	Latest    *Release
	Newer     bool // Latest is newer than Current
	Source    bool // a build from source: installing puts the release in place of the built program
	CanApply  bool // this program can replace itself (not in Docker)
	CheckedAt time.Time
	Err       string
	Off       bool // checking is turned off (UPDATE_CHECK=off)
}

// Updater checks for releases and installs them.
type Updater struct {
	current string
	api     string // the GitHub API base, swapped in tests
	client  *http.Client
	log     *slog.Logger
	off     bool
	exe     func() (string, error)

	mu      sync.Mutex
	latest  *Release
	checked time.Time
	err     string
	busy    bool
}

func New(current string, off bool, log *slog.Logger) *Updater {
	return &Updater{current: current, api: "https://api.github.com", client: &http.Client{Timeout: 5 * time.Minute},
		log: log, off: off, exe: os.Executable}
}

// version is a release version: v1.2.3.
type version [3]int

// parse reads "v1.2.3"; ok is false for anything else, such as "dev" or "v1.2.3-4-gabc".
func parse(v string) (version, bool) {
	var out version
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 || !strings.HasPrefix(v, "v") {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// base is the release a source build comes from: "v1.2.3-4-gabc-dirty" → v1.2.3.
func base(v string) (version, bool) {
	if i := strings.IndexByte(v, '-'); i > 0 {
		v = v[:i]
	}
	return parse(v)
}

func (a version) less(b version) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Newer reports whether tag is a newer release than current. A source build compares by the
// release it comes from; "dev" is older than any release.
func Newer(current, tag string) bool {
	t, ok := parse(tag)
	if !ok {
		return false
	}
	c, ok := base(current)
	if !ok {
		return true
	}
	return c.less(t)
}

func inDocker() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, release := parse(u.current)
	st := Status{Current: u.current, Latest: u.latest, CheckedAt: u.checked, Err: u.err, Off: u.off,
		Source: !release, CanApply: !inDocker()}
	if u.latest != nil {
		st.Newer = Newer(u.current, u.latest.Tag)
	}
	return st
}

// Check asks GitHub for the latest release.
func (u *Updater) Check(ctx context.Context) error {
	rel, err := u.fetchLatest(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = time.Now()
	if err != nil {
		u.err = err.Error()
		return err
	}
	u.latest, u.err = rel, ""
	if Newer(u.current, rel.Tag) {
		u.log.Info("update: a newer release", "current", u.current, "latest", rel.Tag)
	}
	return nil
}

func (u *Updater) fetchLatest(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.api+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "spendbot/"+u.current)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := u.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("GitHub does not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("GitHub's answer: %w", err)
	}
	if _, ok := parse(rel.Tag); !ok {
		return nil, fmt.Errorf("the latest release has an odd tag %q", rel.Tag)
	}
	return &rel, nil
}

// CheckEvery is how often Run asks GitHub: a new release shows on the page within minutes, and
// 12 requests an hour stay well under GitHub's limit of 60 for a computer without a token.
const CheckEvery = 5 * time.Minute

// Run checks soon after start and then every CheckEvery.
func (u *Updater) Run(ctx context.Context) {
	if u.off {
		return
	}
	wait := 15 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := u.Check(ctx); err != nil && ctx.Err() == nil {
			u.log.Warn("update: check", "err", err)
		}
		wait = CheckEvery
	}
}

// ArchiveName is the release archive for this computer.
func ArchiveName(tag, goos, goarch string) string {
	osName, ext := goos, ".tar.gz"
	switch goos {
	case "darwin":
		osName, ext = "macos", ".zip"
	case "windows":
		ext = ".zip"
	}
	return "spendbot-" + tag + "-" + osName + "-" + goarch + ext
}

// Apply downloads the latest release for this computer, checks it and puts it in place of
// the running program. The old program stays next to it as spendbot.old. It returns the tag
// installed; the caller restarts.
func (u *Updater) Apply(ctx context.Context) (string, error) {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return "", errors.New("an update is already running")
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()

	if !u.Status().CanApply {
		return "", errors.New("this spendbot cannot replace itself here (Docker): pull the new image")
	}
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		return "", err
	}
	if !Newer(u.current, rel.Tag) {
		return "", fmt.Errorf("%s is the latest already", u.current)
	}
	exe, err := u.exe()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	if err := u.install(ctx, rel, runtime.GOOS, runtime.GOARCH, exe); err != nil {
		return "", err
	}
	u.log.Info("update: installed", "from", u.current, "to", rel.Tag)
	return rel.Tag, nil
}

// install downloads, checks and swaps the program at exe.
func (u *Updater) install(ctx context.Context, rel *Release, goos, goarch, exe string) error {
	name := ArchiveName(rel.Tag, goos, goarch)
	var archive, sums *Asset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case name:
			archive = &rel.Assets[i]
		case "SHA256SUMS.txt":
			sums = &rel.Assets[i]
		}
	}
	if archive == nil || sums == nil {
		return fmt.Errorf("release %s has no %s or SHA256SUMS.txt", rel.Tag, name)
	}
	sumText, err := u.download(ctx, sums.URL, 1<<20)
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sumText))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("SHA256SUMS.txt has no line for %s", name)
	}
	data, err := u.download(ctx, archive.URL, maxArchive)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return errors.New("the downloaded archive does not match its checksum: not installed")
	}
	bin := "spendbot"
	if goos == "windows" {
		bin = "spendbot.exe"
	}
	prog, err := extract(data, strings.HasSuffix(name, ".zip"), bin)
	if err != nil {
		return err
	}
	return swap(exe, prog)
}

func (u *Updater) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "spendbot/"+u.current)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", filepath.Base(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("the download is too large")
	}
	return b, nil
}

// extract takes the program out of a release archive: "<folder>/spendbot".
func extract(data []byte, isZip bool, bin string) ([]byte, error) {
	if isZip {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == bin && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return readLimited(rc)
			}
		}
		return nil, fmt.Errorf("%s is not in the archive", bin)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is not in the archive", bin)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == bin && h.Typeflag == tar.TypeReg {
			return readLimited(tr)
		}
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinary+1))
	if err == nil && len(b) > maxBinary {
		err = errors.New("the program in the archive is too large")
	}
	return b, err
}

// swap puts prog in place of exe and keeps the old one as exe.old. Renaming a running program
// works on macOS, Linux and Windows; writing over it does not on Windows.
func swap(exe string, prog []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".spendbot-new-*")
	if err != nil {
		return fmt.Errorf("cannot write next to the program: %w", err)
	}
	if _, err := tmp.Write(prog); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		_ = os.Rename(old, exe) // put the old one back
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Point makes the Updater use another GitHub API and program path: for tests.
func (u *Updater) Point(api string, exe func() (string, error)) {
	u.api, u.exe = api, exe
}

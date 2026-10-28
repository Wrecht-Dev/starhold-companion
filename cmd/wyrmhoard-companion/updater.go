package main

// Simple release-tracking self-updater. On startup (and every 24h while the
// host lives) the companion asks GitHub for the latest published release,
// compares it with the running version, and if newer: downloads the matching
// platform archive, verifies its SHA-256 against the published checksums.txt,
// and swaps the running binary in place. Native messaging hosts restart with
// the browser, so the swap takes effect next session; a playing mpv is never
// touched.
//
// Disable with WYRMHOARD_COMPANION_NO_UPDATE=1 (managed installs).

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releasesAPI = "https://api.github.com/repos/Wrecht-Dev/starhold-companion/releases/latest"
const checkEvery = 24 * time.Hour

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
	Draft   bool      `json:"draft"`
	Prerele bool      `json:"prerelease"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

func updatesDisabled() bool {
	for _, k := range []string{"WYRMHOARD_COMPANION_NO_UPDATE", "WYRMHOARD_COMPANION_SKIP_UPDATE"} {
		if v, ok := os.LookupEnv(k); ok && v != "" && v != "0" && v != "false" {
			return true
		}
	}
	return false
}

func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func selfUpdateLoop() {
	if updatesDisabled() {
		return
	}
	go func() {
		time.Sleep(15 * time.Second) // let pairing settle before network chatter
		for {
			if err := selfUpdate(); err != nil {
				fmt.Fprintln(os.Stderr, "update:", err)
			}
			time.Sleep(checkEvery)
		}
	}()
}

func selfUpdate() error {
	latest, err := latestRelease()
	if err != nil {
		return err
	}
	if latest == "" || compareSemver(latest, version) <= 0 {
		return nil // up to date
	}
	fmt.Fprintf(os.Stderr, "update: %s -> %s available\n", version, latest)
	bin, err := exePath()
	if err != nil {
		return err
	}
	want := fmt.Sprintf("starhold-companion-%s-%s", runtime.GOOS, runtime.GOARCH)
	var arch, sums *ghAsset
	for i := range latest2.Assets {
		a := &latest2.Assets[i]
		switch {
		case strings.HasPrefix(a.Name, want) && (strings.HasSuffix(a.Name, ".tar.gz") || strings.HasSuffix(a.Name, ".zip")):
			arch = a
		case a.Name == "checksums.txt":
			sums = a
		}
	}
	if arch == nil {
		return errors.New("no release asset for this platform")
	}
	tmp, err := os.MkdirTemp("", "whc-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, arch.Name)
	if err := download(arch.URL, file, arch.Size); err != nil {
		return err
	}
	if sums != nil {
		sumFile := filepath.Join(tmp, "checksums.txt")
		if err := download(sums.URL, sumFile, sums.Size); err != nil {
			return err
		}
		ok, err := verifySum(file, sumFile, arch.Name)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("checksum mismatch; refusing update")
		}
	}
	newBin, err := unpackBinary(file, tmp)
	if err != nil {
		return err
	}
	return swapBinary(newBin, bin)
}

var latest2 *ghRelease // set by latestRelease; kept tiny to avoid re-fetch plumbing

func latestRelease() (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, releasesAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.Draft || rel.Prerele {
		return "", nil
	}
	latest2 = &rel
	return strings.TrimPrefix(rel.TagName, "v"), nil
}

func download(url, dest string, size int64) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return err
	}
	return w.Flush()
}

func verifySum(file, sumFile, name string) (bool, error) {
	f, err := os.Open(sumFile)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var want string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasSuffix(line, name) {
			want = strings.Fields(line)[0]
			break
		}
	}
	if want == "" {
		return false, errors.New("checksums.txt has no entry for " + name)
	}
	h := sha256.New()
	in, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer in.Close()
	if _, err := io.Copy(h, in); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// unpackBinary extracts the companion binary from the release archive.
func unpackBinary(archive, dir string) (string, error) {
	if strings.HasSuffix(archive, ".zip") {
		return unpackZip(archive, dir)
	}
	return unpackTarGz(archive, dir)
}

func unpackTarGz(archive, dir string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("binary not found in archive")
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(hdr.Name) == "wyrmhoard-companion" && hdr.Typeflag == tar.TypeReg {
			out := filepath.Join(dir, "wyrmhoard-companion.new")
			o, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(o, tr); err != nil {
				o.Close()
				return "", err
			}
			o.Close()
			return out, nil
		}
	}
}

func unpackZip(archive, dir string) (string, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if filepath.Base(zf.Name) == "wyrmhoard-companion.exe" {
			rc, err := zf.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			out := filepath.Join(dir, "wyrmhoard-companion.new.exe")
			o, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(o, rc); err != nil {
				o.Close()
				return "", err
			}
			o.Close()
			return out, nil
		}
	}
	return "", errors.New("binary not found in archive")
}

// swapBinary moves new over running. POSIX rename is atomic; Windows allows
// renaming a running exe, so the old image is parked out of the way first.
func swapBinary(newBin, cur string) error {
	if runtime.GOOS == "windows" {
		old := cur + ".old"
		_ = os.Remove(old)
		if err := os.Rename(cur, old); err != nil {
			return err
		}
		if err := os.Rename(newBin, cur); err != nil {
			_ = os.Rename(old, cur) // restore rather than vanish
			return err
		}
		_ = os.Remove(old)
		return nil
	}
	if err := os.Rename(newBin, cur); err != nil {
		return err
	}
	return os.Chmod(cur, 0o755)
}

// compareSemver compares dotted numeric versions ("0.2.1" vs "0.1.0").
func compareSemver(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		ai, bi := atoi0(seg(as, i)), atoi0(seg(bs, i))
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

func seg(v []string, i int) string {
	if i < len(v) {
		return v[i]
	}
	return "0"
}

func atoi0(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

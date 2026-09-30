package torbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const indexURL = "https://www.torproject.org/download/tor/"
const maxArchiveBytes = 128 << 20

var bundleLink = regexp.MustCompile(`https://dist\.torproject\.org/torbrowser/([0-9]+(?:\.[0-9]+)+)/tor-expert-bundle-windows-x86_64-([0-9]+(?:\.[0-9]+)+)\.tar\.gz`)

func versionParts(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, _ := strconv.Atoi(part)
		out = append(out, n)
	}
	return out
}

func versionGreater(a, b string) bool {
	x, y := versionParts(a), versionParts(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		v, w := 0, 0
		if i < len(x) {
			v = x[i]
		}
		if i < len(y) {
			w = y[i]
		}
		if v != w {
			return v > w
		}
	}
	return false
}

func latestStableWindowsBundle(page []byte) (string, string, error) {
	version, bundleURL := "", ""
	for _, match := range bundleLink.FindAllStringSubmatch(string(page), -1) {
		if match[1] == match[2] && (version == "" || versionGreater(match[1], version)) {
			version, bundleURL = match[1], match[0]
		}
	}
	if version == "" {
		return "", "", errors.New("Tor Project page has no stable Windows Expert Bundle")
	}
	return version, bundleURL, nil
}

func download(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("Tor download exceeded size limit")
	}
	return body, nil
}

func checksumForFile(list []byte, filename string) (string, error) {
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == filename && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", errors.New("Tor checksum is missing")
}

func extractVerifiedBundle(dir string, archive []byte, wantHash string) (string, error) {
	hash := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(hash[:]), wantHash) {
		return "", errors.New("Tor bundle checksum mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	total := int64(0)
	var binary string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Size < 0 || hdr.Size > maxArchiveBytes {
			return "", errors.New("invalid Tor bundle member size")
		}
		total += hdr.Size
		if total > 256<<20 {
			return "", errors.New("Tor bundle expands beyond size limit")
		}
		name := filepath.FromSlash(hdr.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") || strings.Contains(name, ":") {
			return "", errors.New("invalid Tor bundle path")
		}
		target := filepath.Join(dir, name)
		rel, err := filepath.Rel(dir, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", errors.New("Tor bundle path escapes directory")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return "", err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return "", err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
			if err != nil {
				return "", err
			}
			_, copyErr := io.CopyN(file, tr, hdr.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			if strings.EqualFold(filepath.Base(target), "tor.exe") {
				binary = target
			}
		default:
			return "", errors.New("unsupported Tor bundle member")
		}
	}
	if binary == "" {
		return "", errors.New("Tor bundle did not contain tor.exe")
	}
	return binary, nil
}

// Ensure downloads the latest stable official Expert Bundle when the locally
// installed version differs. An existing verified installation stays usable
// when the Tor Project is temporarily unreachable.
func Ensure(ctx context.Context, root string) (string, string, error) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return "", "", errors.New("automatic Tor bundle is currently supported on Windows x64")
	}
	client := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
	}}
	current, _ := os.ReadFile(filepath.Join(root, "current.txt"))
	oldVersion := strings.TrimSpace(string(current))
	oldBinary := filepath.Join(root, oldVersion, "tor", "tor.exe")
	if _, err := os.Stat(oldBinary); err != nil {
		oldBinary = ""
	}
	page, err := download(ctx, client, indexURL, 2<<20)
	if err != nil {
		if oldBinary != "" {
			return oldBinary, oldVersion, nil
		}
		return "", "", fmt.Errorf("Tor update check: %w", err)
	}
	version, bundleURL, err := latestStableWindowsBundle(page)
	if err != nil {
		if oldBinary != "" {
			return oldBinary, oldVersion, nil
		}
		return "", "", err
	}
	if version == oldVersion && oldBinary != "" {
		return oldBinary, version, nil
	}
	filename := filepath.Base(bundleURL)
	checksumURL := strings.TrimSuffix(bundleURL, filename) + "sha256sums-signed-build.txt"
	list, err := download(ctx, client, checksumURL, 1<<20)
	if err != nil {
		return oldBinary, oldVersion, fmt.Errorf("Tor checksums: %w", err)
	}
	wantHash, err := checksumForFile(list, filename)
	if err != nil {
		return oldBinary, oldVersion, err
	}
	archive, err := download(ctx, client, bundleURL, maxArchiveBytes)
	if err != nil {
		return oldBinary, oldVersion, fmt.Errorf("Tor bundle: %w", err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return oldBinary, oldVersion, err
	}
	stage, err := os.MkdirTemp(root, "tor-stage-")
	if err != nil {
		return oldBinary, oldVersion, err
	}
	defer os.RemoveAll(stage)
	binary, err := extractVerifiedBundle(stage, archive, wantHash)
	if err != nil {
		return oldBinary, oldVersion, err
	}
	installed := filepath.Join(root, version)
	if err := os.Rename(stage, installed); err != nil {
		if _, statErr := os.Stat(installed); statErr != nil {
			return oldBinary, oldVersion, err
		}
	}
	newBinary := filepath.Join(installed, strings.TrimPrefix(binary, stage+string(os.PathSeparator)))
	if _, err := os.Stat(newBinary); err != nil {
		return oldBinary, oldVersion, err
	}
	if err := os.WriteFile(filepath.Join(root, "current.txt"), []byte(version), 0600); err != nil {
		return oldBinary, oldVersion, err
	}
	return newBinary, version, nil
}

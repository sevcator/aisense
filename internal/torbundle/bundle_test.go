package torbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestLatestStableWindowsBundleSkipsAlpha(t *testing.T) {
	html := `<a href="https://dist.torproject.org/torbrowser/15.0.20/tor-expert-bundle-windows-x86_64-15.0.20.tar.gz">old</a>
<a href="https://dist.torproject.org/torbrowser/16.0a12/tor-expert-bundle-windows-x86_64-16.0a12.tar.gz">alpha</a>
<a href="https://dist.torproject.org/torbrowser/15.0.24/tor-expert-bundle-windows-x86_64-15.0.24.tar.gz">current</a>`
	version, bundleURL, err := latestStableWindowsBundle([]byte(html))
	if err != nil || version != "15.0.24" || bundleURL != "https://dist.torproject.org/torbrowser/15.0.24/tor-expert-bundle-windows-x86_64-15.0.24.tar.gz" {
		t.Fatalf("got %q %q %v", version, bundleURL, err)
	}
}

func TestOfficialBundleDownload(t *testing.T) {
	if os.Getenv("AISENSE_TEST_TOR_DOWNLOAD") != "1" {
		t.Skip("requires Tor Project download")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	binary, version, err := Ensure(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if version == "" {
		t.Fatal("missing installed version")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	again, againVersion, err := Ensure(ctx, root)
	if err != nil || again != binary || againVersion != version {
		t.Fatalf("repeat ensure: %q %q %v", again, againVersion, err)
	}
}

func TestExtractVerifiedBundleRejectsTraversalAndChecksHash(t *testing.T) {
	makeArchive := func(name string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		payload := []byte("fake tor binary")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	archive := makeArchive("tor/tor.exe")
	hash := sha256.Sum256(archive)
	if _, err := extractVerifiedBundle(t.TempDir(), archive, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := extractVerifiedBundle(t.TempDir(), archive, "invalid hash"); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
	bad := makeArchive("../escape.exe")
	badHash := sha256.Sum256(bad)
	if _, err := extractVerifiedBundle(t.TempDir(), bad, hex.EncodeToString(badHash[:])); err == nil {
		t.Fatal("archive path traversal was accepted")
	}
}

package vault

import (
	"bytes"
	"strings"
	"testing"
)

func TestSealedDataRoundTripsAndHidesItsContents(t *testing.T) {
	plain := []byte(`{"api_keys":[{"key":"sk-secret-value"}]}`)
	sealed, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-secret-value")) {
		t.Fatal("the secret is still readable in the sealed file")
	}
	if !IsSealed(sealed) || IsSealed(plain) {
		t.Fatal("sealed files must be recognizable")
	}
	opened, err := Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plain) {
		t.Fatalf("round trip = %q", opened)
	}
	// Every save uses a fresh key and nonce.
	again, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed, again) {
		t.Fatal("two saves produced the same bytes")
	}
}

func TestChangedSealedFileIsRefused(t *testing.T) {
	sealed, err := Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	broken := append([]byte(nil), sealed...)
	broken[len(broken)-1] ^= 0xff
	if _, err := Open(broken); err == nil {
		t.Fatal("a changed file must not open")
	}
	if _, err := Open(sealed[:len(sealed)-5]); err == nil {
		t.Fatal("a truncated file must not open")
	}
	if _, err := Open([]byte("plain text")); err == nil || !strings.Contains(err.Error(), "not a sealed") {
		t.Fatalf("plain text err = %v", err)
	}
}

func TestPlainFilesStillLoadForMigration(t *testing.T) {
	plain := []byte(`{"upstreams":[]}`)
	opened, sealed, err := OpenOrPlain(plain)
	if err != nil || sealed || !bytes.Equal(opened, plain) {
		t.Fatalf("plain: %q sealed=%v err=%v", opened, sealed, err)
	}
	box, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	opened, sealed, err = OpenOrPlain(box)
	if err != nil || !sealed || !bytes.Equal(opened, plain) {
		t.Fatalf("sealed: %q sealed=%v err=%v", opened, sealed, err)
	}
}

func TestEmptyAndLargePayloads(t *testing.T) {
	for _, size := range []int{0, 1, 1 << 20} {
		plain := bytes.Repeat([]byte("a"), size)
		sealed, err := Seal(plain)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		opened, err := Open(sealed)
		if err != nil || !bytes.Equal(opened, plain) {
			t.Fatalf("size %d: err=%v", size, err)
		}
	}
}

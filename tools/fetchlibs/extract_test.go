package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarGzMember(t *testing.T) {
	archive := makeTarGz(t, map[string]string{
		"libtokenizers.a": "STATIC-LIB-BYTES",
		"README":          "ignore me",
	})
	dst := filepath.Join(t.TempDir(), "out.a")
	if err := extractTarGzMember(dst, "libtokenizers.a", bytes.NewReader(archive)); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "STATIC-LIB-BYTES" {
		t.Errorf("content = %q, want STATIC-LIB-BYTES", got)
	}
}

func TestExtractTarGzMemberMissing(t *testing.T) {
	archive := makeTarGz(t, map[string]string{"other.a": "x"})
	dst := filepath.Join(t.TempDir(), "out.a")
	if err := extractTarGzMember(dst, "libtokenizers.a", bytes.NewReader(archive)); err == nil {
		t.Fatal("expected error when member absent, got nil")
	}
}

func TestExtractZipMember(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "ort.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{
		"onnxruntime-win-x64-1.24.3/lib/onnxruntime.dll": "DLL-BYTES",
		"onnxruntime-win-x64-1.24.3/lib/onnxruntime.lib": "ignore",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(t.TempDir(), "onnxruntime.dll")
	if err := extractZipMember(dst, "onnxruntime-win-x64-1.24.3/lib/onnxruntime.dll", zipPath); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "DLL-BYTES" {
		t.Errorf("content = %q, want DLL-BYTES", got)
	}
}

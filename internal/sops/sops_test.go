package sops

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDetection(t *testing.T) {
	for _, tc := range []struct {
		name, data     string
		encrypted, bad bool
	}{
		{"values.yaml", "password: plain", false, false},
		{"values.yaml", "sops: {age: []}", true, false},
		{"values.sops.yml", "sops: {age: []}", true, false},
		{"values.sops.yaml", "password: accidentally-plain", false, true},
		{"values.yaml", "sops: null", false, true},
		{"values.yaml", "nested: {sops: {age: []}}", false, false},
		{"values.yaml", "sops: {}", false, true},
		{"values.yaml", "password: [secret-value", false, true},
		{".sops.yaml", "creation_rules: []", false, false},
	} {
		got, err := Encrypted(tc.name, []byte(tc.data))
		if got != tc.encrypted || (err != nil) != tc.bad {
			t.Errorf("%s %s: %v %v", tc.name, tc.data, got, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-value") {
			t.Fatal("error leaked input")
		}
	}
}
func TestDecryptCancellationAndMissingCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("test identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := Decrypt(context.Background(), "file.yaml", key); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Decrypt(ctx, "file.yaml", key); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatal(err)
	}
}

// Package sops detects encrypted values and drives the SOPS CLI. Plaintext is
// read only from stdout into memory and is never written to a checkout.
package sops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Named identifies encrypted values by suffix, excluding SOPS' own rules file.
func Named(name string) bool {
	name = filepath.Base(name)
	return name != ".sops.yaml" && (strings.HasSuffix(name, ".sops.yaml") || strings.HasSuffix(name, ".sops.yml"))
}

// Encrypted checks top-level metadata, requiring it for a named encrypted file.
// YAML parser diagnostics are deliberately omitted: they can quote value data.
func Encrypted(name string, data []byte) (bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("%s: invalid YAML", name)
	}
	found := false
	if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
		n := doc.Content[0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "sops" {
				if found || n.Content[i+1].Kind != yaml.MappingNode || len(n.Content[i+1].Content) == 0 {
					return false, fmt.Errorf("%s: invalid SOPS metadata", name)
				}
				found = true
			}
		}
	}
	if Named(name) && !found {
		return false, fmt.Errorf("%s: encrypted values file has no SOPS metadata", name)
	}
	return found, nil
}

// Decrypt requires an explicitly configured age identity. Inherited SOPS age
// variables are removed so inherited age commands and inline keys cannot
// override the identity selected in composecd.yaml.
func Decrypt(ctx context.Context, file, keyFile string) ([]byte, error) {
	if keyFile == "" {
		return nil, errors.New("sopsAgeKeyFile is required to decrypt this file")
	}
	f, err := os.Open(keyFile)
	if err != nil {
		return nil, errors.New("cannot read sopsAgeKeyFile")
	}
	st, err := f.Stat()
	_ = f.Close()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("sopsAgeKeyFile must be a readable regular file")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sops", "--decrypt", "--output-type", "yaml", "--input-type", "yaml", file)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "SOPS_AGE_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "SOPS_AGE_KEY_FILE="+keyFile)
	// Never include stderr or partial stdout in an error: either may contain
	// values, an age identity, or details about another decryption backend.
	cmd.Stderr = io.Discard
	var out limitedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sops decryption canceled: %w", ctx.Err())
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("sops is not installed or is not on PATH")
		}
		return nil, errors.New("sops decryption failed; check the age identity and encrypted file")
	}
	return out.Bytes(), nil
}

// Bound a corrupt file or subprocess without ever spilling plaintext to disk.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<20 {
		return 0, errors.New("decrypted values exceed 16 MiB")
	}
	return b.Buffer.Write(p)
}

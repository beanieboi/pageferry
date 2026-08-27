package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"unknown": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseExtendedDuration(t *testing.T) {
	d, err := ParseExtendedDuration("30d")
	if err != nil {
		t.Fatal(err)
	}
	if d != 30*24*time.Hour {
		t.Fatalf("got %s", d)
	}
}

func TestPaperlessCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	data := "# credentials\nPAPERLESS_URL='https://paperless.example.net'\nPAPERLESS_TOKEN=secret-value\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.PaperlessEnvFile = path
	baseURL, token, err := c.PaperlessCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if baseURL != "https://paperless.example.net" || token != "secret-value" {
		t.Fatalf("unexpected credentials: url=%q token length=%d", baseURL, len(token))
	}
}

func TestPaperlessCredentialsRejectsDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	data := "PAPERLESS_URL=https://one\nPAPERLESS_URL=https://two\nPAPERLESS_TOKEN=secret\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.PaperlessEnvFile = path
	if _, _, err := c.PaperlessCredentials(); err == nil {
		t.Fatal("expected duplicate-key error")
	}
}

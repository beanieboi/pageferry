package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	SpoolDir         string `json:"spool_dir"`
	PaperlessEnvFile string `json:"paperless_env_file"`

	Device        string   `json:"device"`
	SANEConfigDir string   `json:"sane_config_dir"`
	Source        string   `json:"source"`
	Mode          string   `json:"mode"`
	ResolutionDPI int      `json:"resolution_dpi"`
	PageWidthMM   int      `json:"page_width_mm"`
	PageHeightMM  int      `json:"page_height_mm"`
	PDFQuality    int      `json:"pdf_quality"`
	ScanOptions   []string `json:"scan_options"`

	ScanimagePath string `json:"scanimage_path"`
	TIFFCPPath    string `json:"tiffcp_path"`
	TIFF2PDFPath  string `json:"tiff2pdf_path"`
	PDFInfoPath   string `json:"pdfinfo_path"`

	MinFreeBytes       uint64 `json:"min_free_bytes"`
	PollInterval       string `json:"poll_interval"`
	RetryInitial       string `json:"retry_initial"`
	RetryMaximum       string `json:"retry_maximum"`
	HTTPTimeout        string `json:"http_timeout"`
	CompletedRetention string `json:"completed_retention"`
}

func Default() Config {
	return Config{
		SpoolDir:           "/var/lib/pageferry",
		PaperlessEnvFile:   "/etc/pageferry/paperless.env",
		SANEConfigDir:      "/etc/scanbd",
		Source:             "ADF Duplex",
		Mode:               "Color",
		ResolutionDPI:      300,
		PageWidthMM:        210,
		PageHeightMM:       297,
		PDFQuality:         85,
		ScanimagePath:      "/usr/bin/scanimage",
		TIFFCPPath:         "/usr/bin/tiffcp",
		TIFF2PDFPath:       "/usr/bin/tiff2pdf",
		PDFInfoPath:        "/usr/bin/pdfinfo",
		MinFreeBytes:       1 << 30,
		PollInterval:       "5s",
		RetryInitial:       "30s",
		RetryMaximum:       "30m",
		HTTPTimeout:        "10m",
		CompletedRetention: "30d",
	}
}

func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode config: trailing JSON data")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.SpoolDir == "" || c.Source == "" || c.Mode == "" {
		return errors.New("spool_dir, source, and mode must not be empty")
	}
	if c.ResolutionDPI < 50 || c.ResolutionDPI > 1200 {
		return errors.New("resolution_dpi must be between 50 and 1200")
	}
	if c.PageWidthMM <= 0 || c.PageHeightMM <= 0 {
		return errors.New("page dimensions must be positive")
	}
	if c.PDFQuality < 1 || c.PDFQuality > 100 {
		return errors.New("pdf_quality must be between 1 and 100")
	}
	for _, option := range c.ScanOptions {
		if option == "" || strings.HasPrefix(option, "--batch") || strings.HasPrefix(option, "--device-name") || strings.HasPrefix(option, "--format") {
			return fmt.Errorf("scan_options contains an empty or managed option %q", option)
		}
	}
	for name, value := range map[string]string{
		"poll_interval": c.PollInterval,
		"retry_initial": c.RetryInitial,
		"retry_maximum": c.RetryMaximum,
		"http_timeout":  c.HTTPTimeout,
	} {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s must be a positive Go duration", name)
		}
	}
	if _, err := ParseExtendedDuration(c.CompletedRetention); err != nil {
		return fmt.Errorf("completed_retention: %w", err)
	}
	return nil
}

func (c Config) Duration(value string) time.Duration {
	d, _ := time.ParseDuration(value)
	return d
}

func ParseExtendedDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days := strings.TrimSuffix(value, "d")
		d, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, errors.New("must be a duration such as 72h or 30d")
		}
		return d * 24, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return 0, errors.New("must be a non-negative duration such as 72h or 30d")
	}
	return d, nil
}

func (c Config) PaperlessCredentials() (baseURL, token string, returnErr error) {
	info, err := os.Stat(c.PaperlessEnvFile)
	if err != nil {
		return "", "", fmt.Errorf("stat Paperless environment file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("Paperless environment path is not a regular file")
	}
	if info.Mode().Perm()&0o007 != 0 {
		return "", "", errors.New("Paperless environment file must not be accessible by other users")
	}
	f, err := os.Open(c.PaperlessEnvFile)
	if err != nil {
		return "", "", fmt.Errorf("open Paperless environment file: %w", err)
	}
	defer f.Close()
	values := make(map[string]string, 2)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, rawValue, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || (key != "PAPERLESS_URL" && key != "PAPERLESS_TOKEN") {
			return "", "", fmt.Errorf("Paperless environment line %d has an unknown or malformed key", lineNumber)
		}
		if _, exists := values[key]; exists {
			return "", "", fmt.Errorf("Paperless environment line %d duplicates %s", lineNumber, key)
		}
		value, err := parseEnvValue(rawValue)
		if err != nil {
			return "", "", fmt.Errorf("Paperless environment line %d: %w", lineNumber, err)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("read Paperless environment file: %w", err)
	}
	baseURL = strings.TrimSpace(values["PAPERLESS_URL"])
	token = strings.TrimSpace(values["PAPERLESS_TOKEN"])
	if baseURL == "" || token == "" {
		return "", "", errors.New("Paperless environment file requires non-empty PAPERLESS_URL and PAPERLESS_TOKEN")
	}
	return baseURL, token, nil
}

func parseEnvValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("unterminated single-quoted value")
		}
		return value[1 : len(value)-1], nil
	}
	if value[0] == '"' {
		if len(value) < 2 || value[len(value)-1] != '"' {
			return "", errors.New("unterminated double-quoted value")
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", errors.New("invalid double-quoted value")
		}
		return decoded, nil
	}
	return value, nil
}

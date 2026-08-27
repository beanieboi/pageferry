package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/beanieboi/pageferry/internal/capture"
	"github.com/beanieboi/pageferry/internal/config"
	"github.com/beanieboi/pageferry/internal/paperless"
	"github.com/beanieboi/pageferry/internal/spool"
	"github.com/beanieboi/pageferry/internal/worker"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "pageferry-button" && len(args) == 0 {
		args = []string{"capture"}
	}
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, logger, args); err != nil {
		logger.Error("command failed", "command", args[0], "error", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, logger *slog.Logger, args []string) error {
	switch args[0] {
	case "capture":
		return captureCommand(ctx, logger, args[1:])
	case "run":
		return runCommand(ctx, logger, args[1:])
	case "check":
		return checkCommand(ctx, args[1:])
	case "status":
		return statusCommand(args[1:])
	case "retry":
		return retryCommand(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func checkCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	path := flags.String("config", configPath(), "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	c, _, err := load(*path)
	if err != nil {
		return err
	}
	baseURL, token, err := c.PaperlessCredentials()
	if err != nil {
		return err
	}
	api, err := paperless.New(baseURL, token, c.Duration(c.HTTPTimeout))
	if err != nil {
		return err
	}
	if _, _, err := api.LookupTask(ctx, "pageferry-connectivity-check"); err != nil {
		return err
	}
	fmt.Println("Paperless API and task tracking are available")
	return nil
}

func captureCommand(ctx context.Context, logger *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("capture", flag.ContinueOnError)
	path := flags.String("config", configPath(), "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	c, store, err := load(*path)
	if err != nil {
		return err
	}
	job, err := (&capture.Capturer{Config: c, Store: store, Logger: logger}).Run(ctx)
	if err != nil {
		return err
	}
	fmt.Println(job.ID)
	return nil
}

func runCommand(ctx context.Context, logger *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	path := flags.String("config", configPath(), "configuration file")
	once := flags.Bool("once", false, "process the queue once and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	c, store, err := load(*path)
	if err != nil {
		return err
	}
	baseURL, token, err := c.PaperlessCredentials()
	if err != nil {
		return err
	}
	api, err := paperless.New(baseURL, token, c.Duration(c.HTTPTimeout))
	if err != nil {
		return err
	}
	w := &worker.Worker{Store: store, API: api, Logger: logger, PollInterval: c.Duration(c.PollInterval), RetryInitial: c.Duration(c.RetryInitial), RetryMaximum: c.Duration(c.RetryMaximum)}
	retention, _ := config.ParseExtendedDuration(c.CompletedRetention)
	if err := store.PruneCompleted(retention); err != nil {
		logger.Warn("could not prune old completion receipts", "error", err)
	}
	if *once {
		return w.ProcessOnce(ctx)
	}
	logger.Info("uploader started", "spool_dir", c.SpoolDir, "paperless_url", baseURL)
	return w.Run(ctx)
}

func statusCommand(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	path := flags.String("config", configPath(), "configuration file")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	_, store, err := load(*path)
	if err != nil {
		return err
	}
	jobs, err := store.List()
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(jobs)
	}
	if len(jobs) == 0 {
		fmt.Println("queue empty")
		return nil
	}
	for _, job := range jobs {
		fmt.Printf("%s  %-10s  pages=%d  attempts=%d  task=%s  error=%s\n", job.ID, job.State, job.Pages, job.UploadAttempts, dash(job.TaskID), dash(job.LastError))
	}
	return nil
}

func retryCommand(args []string) error {
	flags := flag.NewFlagSet("retry", flag.ContinueOnError)
	path := flags.String("config", configPath(), "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: pageferry retry [--config PATH] JOB_ID")
	}
	_, store, err := load(*path)
	if err != nil {
		return err
	}
	return store.Retry(flags.Arg(0))
}

func load(path string) (config.Config, *spool.Store, error) {
	c, err := config.Load(path)
	if err != nil {
		return config.Config{}, nil, err
	}
	store, err := spool.Open(c.SpoolDir)
	return c, store, err
}

func configPath() string {
	if value := os.Getenv("PAGEFERRY_CONFIG"); value != "" {
		return value
	}
	return "/etc/pageferry/config.json"
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return strings.ReplaceAll(value, "\n", " ")
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: pageferry COMMAND

Commands:
  capture   Scan every page in the ADF and durably enqueue one PDF
  run       Upload queued PDFs and track Paperless tasks
  check     Verify Paperless authentication and task tracking
  status    Show queued, submitted, and failed jobs
  retry     Retry a failed Paperless job
  version   Print the build version`)
}

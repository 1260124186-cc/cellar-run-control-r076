package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/config"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/migrate"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/workflowcheck"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fermentctl:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return usageError()
	}
	switch arguments[0] {
	case "serve":
		if len(arguments) != 1 {
			return usageError()
		}
		return serve()
	case "check":
		if len(arguments) != 2 {
			return usageError()
		}
		return workflowcheck.Run(arguments[1])
	case "migrate":
		if len(arguments) != 2 {
			return usageError()
		}
		return migrateCommand(arguments[1])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New(
		"usage: fermentctl serve | fermentctl check <workflow> | fermentctl migrate <status|run|rollback>")
}

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	report, err := migrate.EnsureCurrent(cfg.DataDir)
	if err != nil {
		return err
	}
	logMigration(report)
	store, err := storage.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           httpapi.NewHandler(app),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}()

	log.Printf("cellar run control listening on %s with data %s", cfg.Address, cfg.DataDir)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve HTTP: %w", err)
}

// migrateCommand runs the operator-facing migration subcommands against the
// configured data directory. `run` performs the same upgrade that `serve`
// applies at startup, so the upgrade can be executed and inspected inside a
// maintenance window without opening the HTTP port.
func migrateCommand(action string) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	switch action {
	case "status":
		return printMigrationStatus(cfg.DataDir)
	case "run":
		report, err := migrate.EnsureCurrent(cfg.DataDir)
		if err != nil {
			printMigrationReport(report)
			return err
		}
		printMigrationReport(report)
		return nil
	case "rollback":
		report, err := migrate.Rollback(cfg.DataDir)
		if err != nil {
			return err
		}
		switch report.Action {
		case "none":
			fmt.Printf("no interrupted migration; data version %d\n", report.DataVersion)
		case "discarded":
			fmt.Printf("rolled back migration %s: discarded migration artifacts; data remains at version %d\n",
				report.MigrationID, report.DataVersion)
		case "restored":
			fmt.Printf("rolled back migration %s: restored %s; data version %d\n",
				report.MigrationID, report.BackupFile, report.DataVersion)
		}
		return nil
	default:
		return usageError()
	}
}

func printMigrationStatus(dataDir string) error {
	status := migrate.Inspect(dataDir)
	fmt.Printf("data directory: %s\n", status.DataDir)
	fmt.Printf("binary schema version: %d\n", status.BinaryVersion)
	if !status.StateExists {
		if status.Journal != nil {
			fmt.Println("state file: missing while a migration is interrupted")
		} else {
			fmt.Println("state file: none (first write lands at the current version)")
		}
	} else {
		fmt.Printf("state file: present, schema version %d\n", status.DataVersion)
	}
	if status.DetectionError != "" {
		fmt.Printf("detection problem: %s\n", status.DetectionError)
	}
	if len(status.Pending) == 0 {
		fmt.Println("pending migrations: none")
	} else {
		fmt.Println("pending migrations:")
		for _, step := range status.Pending {
			fmt.Printf("  - %s (v%d -> v%d)\n", step.Name, step.FromVersion, step.ToVersion)
		}
	}
	if status.Journal == nil {
		fmt.Println("interrupted migration: none")
		return nil
	}
	journal := status.Journal
	fmt.Printf("interrupted migration: %s (v%d -> v%d, backup %s)\n",
		journal.MigrationID, journal.FromVersion, journal.ToVersion, journal.BackupFile)
	for _, step := range journal.Steps {
		line := fmt.Sprintf("  - %s (v%d -> v%d): %s", step.Name, step.FromVersion, step.ToVersion, step.Status)
		if step.Checksum != "" {
			line += " " + step.Checksum
		}
		if step.Error != "" {
			line += " — " + step.Error
		}
		fmt.Println(line)
	}
	return nil
}

func logMigration(report *migrate.Report) {
	if report == nil || report.InitialVersion == 0 ||
		report.InitialVersion == report.FinalVersion {
		return
	}
	resumed := ""
	if report.Resumed {
		resumed = " (resumed)"
	}
	log.Printf("migrated data from schema v%d to v%d%s", report.InitialVersion, report.FinalVersion, resumed)
	for _, step := range report.Steps {
		log.Printf("migration step %s: v%d -> v%d %s %s",
			step.Name, step.FromVersion, step.ToVersion, step.Status, step.Checksum)
	}
}

func printMigrationReport(report *migrate.Report) {
	if report == nil {
		return
	}
	switch {
	case report.InitialVersion == 0:
		fmt.Println("no state file yet; the first write lands at the current schema version")
	case report.InitialVersion == report.FinalVersion:
		fmt.Printf("data already at schema version %d; nothing to do\n", report.FinalVersion)
	default:
		resumed := ""
		if report.Resumed {
			resumed = " (resumed)"
		}
		fmt.Printf("migrated schema v%d -> v%d%s\n", report.InitialVersion, report.FinalVersion, resumed)
		if report.BackupFile != "" {
			fmt.Printf("pre-migration backup: %s\n", report.BackupFile)
		}
	}
	for _, step := range report.Steps {
		line := fmt.Sprintf("step %s: v%d -> v%d %s", step.Name, step.FromVersion, step.ToVersion, step.Status)
		if step.Checksum != "" {
			line += " " + step.Checksum
		}
		if step.Detail != "" {
			line += " — " + step.Detail
		}
		fmt.Println(line)
	}
}

func init() {
	log.SetFlags(log.LstdFlags | log.LUTC)
}

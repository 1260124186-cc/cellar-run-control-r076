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
		return migrate(arguments[1])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: fermentctl serve | fermentctl check <workflow> | fermentctl migrate <status|run|rollback>")
}

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
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

// migrate runs the operator-facing migration commands used inside a
// downtime window: inspect the persisted version, execute pending
// migrations without serving, or roll back an interrupted attempt.
func migrate(action string) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	switch action {
	case "status":
		status, err := storage.InspectMigration(cfg.DataDir)
		if err != nil {
			return err
		}
		if !status.StateExists {
			fmt.Printf("state: none (fresh directory, writes start at schema v%d)\n",
				status.CurrentVersion)
		} else {
			fmt.Printf("state: schema v%d (binary supports v%d)\n",
				status.StateVersion, status.CurrentVersion)
		}
		if status.JournalActive {
			fmt.Printf("interrupted migration: v%d -> v%d (%s)\n",
				status.JournalFrom, status.JournalTo, status.JournalStatus)
		}
		if status.LastReport != nil && status.LastReport.Performed {
			fmt.Printf("last migration: v%d -> v%d finished at %s\n",
				status.LastReport.FromVersion, status.LastReport.ToVersion,
				status.LastReport.FinishedAt.Format("2006-01-02T15:04:05Z07:00"))
			for _, step := range status.LastReport.Steps {
				fmt.Printf("  step %s: schema v%d, formulas=%d vessels=%d runs=%d, validated=%t, checksum %s -> %s\n",
					step.Name, step.Version, step.Formulas, step.Vessels, step.Runs,
					step.Validated, step.PreChecksum, step.PostChecksum)
			}
		}
		return nil
	case "run":
		if err := cfg.EnsureDataDir(); err != nil {
			return err
		}
		report, err := storage.Migrate(cfg.DataDir)
		if err != nil {
			return err
		}
		if !report.Performed {
			fmt.Printf("state is already at schema v%d; nothing to migrate\n", report.ToVersion)
			return nil
		}
		fmt.Printf("migrated schema v%d -> v%d in %d step(s)\n",
			report.FromVersion, report.ToVersion, len(report.Steps))
		for _, step := range report.Steps {
			fmt.Printf("  step %s: formulas=%d vessels=%d runs=%d, validated=%t, checksum %s -> %s\n",
				step.Name, step.Formulas, step.Vessels, step.Runs,
				step.Validated, step.PreChecksum, step.PostChecksum)
		}
		return nil
	case "rollback":
		report, err := storage.RollbackMigration(cfg.DataDir)
		if err != nil {
			return err
		}
		if report.RestoredBackup {
			fmt.Printf("rolled back to the pre-migration backup at schema v%d\n",
				report.RestoredVersion)
		} else {
			fmt.Printf("discarded the interrupted migration; state remains at schema v%d\n",
				report.RestoredVersion)
		}
		return nil
	default:
		return usageError()
	}
}

func init() {
	log.SetFlags(log.LstdFlags | log.LUTC)
}

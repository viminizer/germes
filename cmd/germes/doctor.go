package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/config"
	"github.com/viminizer/germes/internal/daemon"
	"github.com/viminizer/germes/internal/store"
)

// doctor checks each part Germes needs, without the daemon, and says how to
// fix what is wrong. It returns an error when something is.
func doctor(ctx context.Context, p paths, client *api.Client) error {
	bad := 0
	check := func(name string, problem, fix string) {
		if problem == "" {
			fmt.Printf("✓ %s\n", name)
			return
		}
		bad++
		fmt.Printf("✗ %s: %s\n    fix: %s\n", name, problem, fix)
	}

	check("home "+p.home, writable(p.home), "make it writable by you, or free disk space")

	cfg, err := config.Load(p.config)
	check("config "+p.config, errText(err), "correct the file (it is YAML), or move it away to start from the defaults")

	if err == nil {
		engines := availableEngines(p, cfg)
		problem := ""
		if len(engines) == 0 {
			problem = "no engine can run"
		}
		check("engines "+strings.Join(engines, ", "), problem, "log in with `codex login`, or install Claude Code")
	}

	// Read-only: doctor never creates, updates or moves the database.
	pending, err := store.GetPendingMigrations(context.Background(), p.db)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Printf("· database %s: none yet (the daemon creates it)\n", p.db)
	case store.IsBusy(err):
		// The file is fine; another Germes process (an update's migration) held it.
		check("database "+p.db, err.Error(), "another Germes process is using it; run `germes doctor` again in a moment")
	default:
		check("database "+p.db, errText(err), "move the file away (Germes starts a new one); never edit it")
		if err == nil && pending > 0 {
			fmt.Printf("· database: %d update(s) waiting (the daemon runs them)\n", pending)
		}
	}

	pid, err := daemon.GetLockHolder(p.lock)
	switch {
	case err != nil:
		check("daemon", err.Error(), "run `germes daemon kill`")
	case pid == 0:
		check("daemon: not running (the next `germes` starts it)", "", "")
	default:
		if d, err := ping(ctx, client); err == nil {
			check(fmt.Sprintf("daemon %s, pid %d", d.Version, d.PID), "", "")
		} else {
			check(fmt.Sprintf("daemon, pid %d", pid), "it holds the lock but does not answer (hung)", "run `germes daemon kill`, then `germes`")
		}
	}

	old, _ := filepath.Glob(filepath.Join(p.home, "old", "*"))
	if len(old) > 0 {
		fmt.Printf("· %d old daemon(s) finishing their turns after an update (%s)\n", len(old), filepath.Join(p.home, "old"))
	}
	fmt.Printf("· log: %s\n", p.log)
	if bad > 0 {
		return fmt.Errorf("%d problem(s) found", bad)
	}
	return nil
}

// killDaemon stops the daemon holding the lock, even a hung one: SIGTERM,
// then SIGKILL after a few seconds. The next `germes` starts a fresh
// daemon, which marks the cut turns as interrupted.
func killDaemon(p paths) error {
	pid, err := daemon.GetLockHolder(p.lock)
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Println("no daemon is running")
		return nil
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal daemon %d: %w", pid, err)
		}
		if sig == syscall.SIGKILL {
			// A stopped (frozen) process only dies once it runs again.
			syscall.Kill(pid, syscall.SIGCONT)
		}
		if waitUntil(5*time.Second, func() bool { held, _ := daemon.GetLockHolder(p.lock); return held == 0 }) {
			fmt.Printf("daemon %d stopped (%s)\n", pid, sig)
			return nil
		}
	}
	return fmt.Errorf("daemon %d did not stop", pid)
}

// writable reports why dir cannot be written, or "".
func writable(dir string) string {
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err.Error()
	}
	f.Close()
	os.Remove(f.Name())
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

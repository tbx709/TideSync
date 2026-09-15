package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tidesync/internal/client"
	"tidesync/internal/config"
	"tidesync/internal/logx"
)

// runSync implements `tidesync sync`: one pull job, either once or forever.
func runSync(args []string) error {
	fs := newFlagSet("sync", "Pull a published directory from an agent to this machine.")
	cfgPath := fs.String("config", "", "configuration file (JSON, // comments allowed)")
	once := fs.Bool("once", false, "run a single synchronisation and exit (for cron/Task Scheduler)")
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	bulk := fs.Bool("bulk", false, "use the tar.gz fast path even for incremental runs")
	checkOnly := fs.Bool("check", false, "fetch the manifest, report the plan, change nothing")

	serverURL := fs.String("server", "", "agent URL, e.g. http://192.168.1.10:8787")
	token := fs.String("token", "", "shared secret (or set server.token in the config)")
	remotePath := fs.String("remote", "", "path below the agent root to mirror (default: the whole root)")
	localDir := fs.String("local", "", "destination directory")
	interval := fs.String("interval", "", "delay between runs in daemon mode, e.g. 5m")
	schedule := fs.String("schedule", "", "comma separated wall clock times, e.g. 02:30,14:30")
	concurrency := fs.Int("concurrency", 0, "parallel downloads")
	verify := fs.String("verify", "", "sha256 | size-mtime | none")
	deleteExtra := fs.Bool("delete-extra", false, "delete local files that no longer exist on the agent")
	name := fs.String("name", "", "friendly job name used in logs")
	exclude := multiFlag{}
	fs.Var(&exclude, "exclude", "exclude pattern, repeatable")
	maxFileMB := fs.Int("max-file-size-mb", -1, "skip files larger than this (0 = unlimited)")
	bandwidth := fs.Int("bandwidth-limit-kbps", -1, "throttle downloads to this rate (0 = unlimited)")

	stateFile := fs.String("state-file", "", "journal location (default: per-user state dir)")
	logLevel := fs.String("log-level", "", "debug | info | warn | error")
	logFile := fs.String("log-file", "", "append logs to this file")
	printCfg := fs.Bool("print-config", false, "print the effective configuration and exit")
	fs.BoolVar(checkOnly, "no-write", false, "alias for -check")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectPositional(fs, "sync"); err != nil {
		return err
	}

	var cfg config.ClientConfig
	loaded := false
	if *cfgPath != "" {
		var err error
		cfg, err = config.LoadClient(*cfgPath)
		if err != nil {
			return err
		}
		loaded = true
	} else {
		cfg = config.ClientDefaults()
	}

	if *serverURL != "" {
		cfg.Server.URL = *serverURL
	}
	if *token != "" {
		cfg.Server.Token = *token
	}
	if *remotePath != "" {
		cfg.Server.RemotePath = *remotePath
	}
	if *localDir != "" {
		cfg.Local.Dir = *localDir
	}
	if *interval != "" {
		d, err := config.ParseDuration(*interval)
		if err != nil {
			return err
		}
		cfg.Sync.Interval = config.D(d)
	}
	if *schedule != "" {
		var list []string
		for _, part := range splitComma(*schedule) {
			list = append(list, part)
		}
		cfg.Sync.Schedule = list
	}
	if *concurrency > 0 {
		cfg.Sync.Concurrency = *concurrency
	}
	if *verify != "" {
		cfg.Verify = *verify
	}
	if *deleteExtra {
		cfg.Local.DeleteExtra = true
	}
	if *name != "" {
		cfg.Name = *name
	}
	if len(exclude) > 0 {
		cfg.Exclude = append(cfg.Exclude, exclude...)
	}
	if *maxFileMB >= 0 {
		cfg.Sync.MaxFileSizeMB = *maxFileMB
	}
	if *bandwidth >= 0 {
		cfg.Sync.BandwidthLimitKBps = *bandwidth
	}
	if *stateFile != "" {
		cfg.StateFile = *stateFile
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *logFile != "" {
		cfg.Log.File = *logFile
	}

	if err := cfg.Validate(); err != nil {
		if !loaded {
			return fmt.Errorf("%w\n\nHint: a sync job needs at least -server, -local and (optionally) -remote.\nRun 'tidesync init client' for a documented example file.", err)
		}
		return err
	}
	if *printCfg {
		return printConfig(cfg)
	}

	log, closeLog, err := logx.New(logx.Options{
		Level: cfg.Log.Level, File: cfg.Log.File,
		MaxSizeMB: cfg.Log.MaxSizeMB, MaxFiles: cfg.Log.MaxFiles, JSON: cfg.Log.JSON,
	})
	if err != nil {
		return err
	}
	defer closeLog()

	for _, note := range cfg.Notes {
		log.Warn(note)
	}
	log.Info("tidesync client starting",
		"version", version, "job", cfg.Name,
		"server", cfg.Server.URL, "remote_path", cfg.Server.RemotePath,
		"local_dir", cfg.Local.Dir, "state_file", cfg.StateFile,
		"interval", cfg.Sync.Interval.String(), "schedule", cfg.Sync.Schedule,
		"concurrency", cfg.Sync.Concurrency, "verify", cfg.Verify,
		"delete_extra", cfg.Local.DeleteExtra, "once", *once,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	syncer, err := client.New(cfg, log)
	if err != nil {
		return err
	}
	if *dryRun || *checkOnly {
		syncer.SetDryRun(true)
	}
	if *bulk {
		syncer.SetForceBulk(true)
	}

	if *once || *dryRun || *checkOnly {
		res, err := runLocked(ctx, cfg, log, syncer)
		if err != nil {
			return err
		}
		if res.Errors > 0 {
			return partialError{msg: fmt.Sprintf("%d file(s) failed to synchronise", res.Errors)}
		}
		return nil
	}

	return runDaemon(ctx, cfg, log, syncer)
}

// runLocked performs one synchronisation under the job lock.
func runLocked(ctx context.Context, cfg config.ClientConfig, log *slog.Logger, syncer *client.Syncer) (*client.RunResult, error) {
	stale := staleLockAfter(cfg)
	lock, err := client.AcquireLock(cfg.LockFile, stale, log)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	// The first run of a job must not wait for a full interval.
	res, err := syncer.RunOnce(ctx)
	if err != nil {
		return res, err
	}
	return res, nil
}

// runDaemon performs the initial run and then keeps running on the configured
// schedule until the process is asked to stop.
func runDaemon(ctx context.Context, cfg config.ClientConfig, log *slog.Logger, syncer *client.Syncer) error {
	if cfg.Sync.Interval <= 0 && len(cfg.Sync.Schedule) == 0 {
		return errors.New("nothing to schedule: set sync.interval, sync.schedule, or run with -once")
	}
	if cfg.Sync.RunOnStart == nil || *cfg.Sync.RunOnStart {
		if _, err := runOnceLogged(ctx, cfg, log, syncer); err != nil && ctx.Err() == nil {
			// A failed run is not fatal: the next tick tries again.
			log.Warn("synchronisation failed, will retry at the next interval", "error", err.Error())
		}
	}
	for {
		next := nextRun(cfg, time.Now())
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		log.Info("next run scheduled", "at", next.Format(time.RFC3339), "in", wait.Round(time.Second).String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Info("stopping on signal")
			return nil
		case <-timer.C:
		}
		if _, err := runOnceLogged(ctx, cfg, log, syncer); err != nil && ctx.Err() == nil {
			log.Warn("synchronisation failed, will retry at the next interval", "error", err.Error())
		}
	}
}

func runOnceLogged(ctx context.Context, cfg config.ClientConfig, log *slog.Logger, syncer *client.Syncer) (*client.RunResult, error) {
	res, err := runLocked(ctx, cfg, log, syncer)
	if err != nil {
		if errors.Is(err, client.ErrLocked) {
			log.Warn("another run holds the lock; skipping this tick")
			return nil, nil
		}
		return res, err
	}
	if res == nil {
		return nil, nil
	}
	if res.Changed() {
		log.Info("changes applied", "downloaded", res.Downloaded, "deleted", res.Deleted, "bytes", res.Bytes)
	} else {
		log.Info("already up to date", "files", res.RemoteFiles)
	}
	return res, nil
}

// nextRun computes the next scheduled run time.
func nextRun(cfg config.ClientConfig, now time.Time) time.Time {
	if times := cfg.ScheduleSeconds(); len(times) > 0 {
		day := now
		for i := 0; i < 2; i++ {
			for _, sec := range times {
				cand := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, now.Location()).Add(time.Duration(sec) * time.Second)
				if cand.After(now) {
					return cand
				}
			}
			day = day.Add(24 * time.Hour)
		}
		return now.Add(24 * time.Hour)
	}
	next := now.Add(cfg.Sync.Interval.Or(5 * time.Minute))
	if j := cfg.Sync.Jitter.Or(0); j > 0 {
		next = next.Add(time.Duration(jitter(j)))
	}
	return next
}

// staleLockAfter decides when a lock file counts as abandoned.
func staleLockAfter(cfg config.ClientConfig) time.Duration {
	base := cfg.Sync.Interval.Or(5*time.Minute) * 3
	if base < 30*time.Minute {
		base = 30 * time.Minute
	}
	return base
}

// jitter returns a random duration in [0, max).
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r == ' ' || r == '\t' {
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

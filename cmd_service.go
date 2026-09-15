package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tidesync/internal/client"
	"tidesync/internal/config"
	"tidesync/internal/logx"
	"tidesync/internal/service"
)

// runService implements `tidesync service ...`.
func runService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tidesync service install|uninstall|status|start|stop|run [-config FILE] [flags]")
	}
	action := args[0]
	fs := newFlagSet("service "+action, "Manage the TideSync background service.")
	cfgPath := fs.String("config", defaultServiceConfig(), "configuration file the service uses")
	role := fs.String("role", "client", "client (pull) or server (publish)")
	interval := fs.String("interval", "", "run interval for scheduled tasks, e.g. 5m or 1h")
	name := fs.String("name", "", "service/task name (default tidesync)")
	mode := fs.String("mode", "auto", "auto | systemd | systemd-user | task | scm")
	user := fs.String("user", "", "Linux only: run the service as this user")
	binary := fs.String("binary", "", "path to the tidesync executable (default: this one)")
	if _, err := parseAll(fs, args[1:]); err != nil {
		return err
	}

	opts := service.Options{
		Role:   service.Role(*role),
		Binary: *binary,
		Config: *cfgPath,
		Name:   *name,
		Mode:   *mode,
		User:   *user,
	}
	if *interval != "" {
		d, err := config.ParseDuration(*interval)
		if err != nil {
			return err
		}
		opts.Interval = d
	} else if *cfgPath != "" {
		if cfg, err := config.LoadClient(*cfgPath); err == nil {
			opts.Interval = cfg.Sync.Interval.Or(5 * time.Minute)
		}
	}

	switch action {
	case "install":
		msg, err := service.Install(opts)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	case "uninstall", "remove":
		msg, err := service.Uninstall(opts)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	case "status":
		info, err := service.Status(opts)
		if err != nil {
			return err
		}
		if !info.Installed {
			fmt.Printf("%s: not installed (%s)\n", opts.NameOrDefault(), info.Detail)
			return nil
		}
		state := "stopped"
		if info.Running {
			state = "running"
		}
		fmt.Printf("%s: %s (%s)\n%s\n", opts.NameOrDefault(), state, info.Manager, info.Detail)
		return nil
	case "start":
		msg, err := service.Start(opts)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	case "stop":
		msg, err := service.Stop(opts)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	case "run":
		return runServiceForeground(opts)
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

// runServiceForeground is the entry point used by a service supervisor. On
// Windows it first tries to attach to the service controller; when that fails
// the job simply runs in the foreground.
func runServiceForeground(opts service.Options) error {
	cfg, err := config.LoadClient(opts.Config)
	if err != nil {
		return err
	}
	log, closeLog, err := logx.New(logx.Options{
		Level: cfg.Log.Level, File: cfg.Log.File,
		MaxSizeMB: cfg.Log.MaxSizeMB, MaxFiles: cfg.Log.MaxFiles, JSON: cfg.Log.JSON,
	})
	if err != nil {
		return err
	}
	defer closeLog()

	work := func(ctx context.Context) error {
		syncer, err := client.New(cfg, log)
		if err != nil {
			return err
		}
		return runDaemon(ctx, cfg, log, syncer)
	}

	asService, err := service.RunInForeground(opts, work)
	if err != nil {
		return err
	}
	if asService {
		return nil
	}
	log.Info("running in the foreground (not started by a service manager)")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return work(ctx)
}

// defaultServiceConfig picks a sensible configuration path when -config is not
// given, so `tidesync service install` works right after `tidesync init`.
func defaultServiceConfig() string {
	candidates := []string{}
	if isWindows() {
		if pd := os.Getenv("ProgramData"); pd != "" {
			candidates = append(candidates, pd+`\TideSync\client.json`)
		}
		candidates = append(candidates, `C:\ProgramData\TideSync\client.json`)
	} else {
		candidates = append(candidates, "/etc/tidesync/client.json")
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, home+"/.config/tidesync/client.json")
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return strings.TrimSpace("client.json")
}

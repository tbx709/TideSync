package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tidesync/internal/config"
	"tidesync/internal/logx"
	"tidesync/internal/proto"
	"tidesync/internal/server"
)

// runServe implements `tidesync serve`.
func runServe(args []string) error {
	fs := newFlagSet("serve", "Publish one directory to the local network, read only.")
	cfgPath := fs.String("config", "", "configuration file (JSON, // comments allowed)")
	root := fs.String("root", "", "directory to publish (overrides server.root)")
	listen := fs.String("listen", "", "listen address, e.g. 0.0.0.0:8787")
	token := fs.String("token", "", "shared secret clients must present")
	genToken := fs.Bool("gen-token", false, "print a fresh random token and exit")
	hashMode := fs.String("hash-mode", "", "auto | always | never")
	exclude := multiFlag{}
	fs.Var(&exclude, "exclude", "exclude pattern, repeatable (e.g. -exclude '*.tmp')")
	allow := multiFlag{}
	fs.Var(&allow, "allow-cidr", "restrict clients to a CIDR or IP, repeatable")
	tlsCert := fs.String("tls-cert", "", "TLS certificate (enables HTTPS)")
	tlsKey := fs.String("tls-key", "", "TLS private key")
	logLevel := fs.String("log-level", "", "debug | info | warn | error")
	logFile := fs.String("log-file", "", "append logs to this file")
	follow := fs.Bool("follow-symlinks", false, "publish what symlinks point at (inside root only)")
	printCfg := fs.Bool("print-config", false, "print the effective configuration and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectPositional(fs, "serve"); err != nil {
		return err
	}

	if *genToken {
		t, err := randomToken()
		if err != nil {
			return err
		}
		fmt.Println(t)
		return nil
	}

	cfg, err := config.LoadServer(*cfgPath)
	if err != nil {
		if *cfgPath != "" || *root == "" {
			return err
		}
		// No config file: build from flags.
		cfg = config.ServerDefaults()
	}
	if *root != "" {
		cfg.Root = *root
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *hashMode != "" {
		cfg.HashMode = *hashMode
	}
	if len(exclude) > 0 {
		cfg.Exclude = append(cfg.Exclude, exclude...)
	}
	if len(allow) > 0 {
		cfg.AllowCIDRs = append(cfg.AllowCIDRs, allow...)
	}
	if *tlsCert != "" {
		cfg.TLSCert = *tlsCert
	}
	if *tlsKey != "" {
		cfg.TLSKey = *tlsKey
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *logFile != "" {
		cfg.Log.File = *logFile
	}
	if *follow {
		cfg.FollowSymlinks = true
	}
	if err := cfg.Validate(); err != nil {
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

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}
	for _, u := range localIPv4Hints(portOf(cfg.Listen)) {
		log.Info("clients on the same network can use this agent URL", "url", u)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			return fmt.Errorf("agent stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down agent")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

// randomToken returns 32 hex characters of cryptographic randomness.
func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// localIPv4Hints lists the addresses a client on the same LAN can use.
func localIPv4Hints(port string) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			out = append(out, "http://"+net.JoinHostPort(ip4.String(), port))
		}
	}
	return out
}

// portOf extracts the port from a listen address for the hint list.
func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strconv.Itoa(proto.DefaultPort)
	}
	return port
}

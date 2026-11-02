// Command intrabin runs an Intrabin node.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/intrabin/intrabin/internal/api"
	"github.com/intrabin/intrabin/internal/audit"
	"github.com/intrabin/intrabin/internal/config"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/monitoring"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
	"github.com/intrabin/intrabin/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version":
			fmt.Printf("intrabin %s (commit %s, built %s)\n", node.Version, node.Commit, node.BuildDate)
			return
		case "reset-password":
			os.Exit(resetPassword(os.Args[2:]))
		case "backup":
			os.Exit(backup(os.Args[2:]))
		case "serve":
		default:
			fmt.Fprintf(os.Stderr, "usage: intrabin [serve|healthcheck|version|reset-password|backup]\n")
			os.Exit(2)
		}
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, cfg.DataDir+"/intrabin.db")
	if err != nil {
		return err
	}
	defer store.Close()
	key, created, err := security.LoadOrCreateMasterKey(cfg.MasterKey, cfg.DataDir)
	if err != nil {
		return err
	}
	if created {
		log.Warn("generated a new master key in the data directory; back it up together with the database", "path", cfg.DataDir+"/master.key")
	}
	box, err := security.NewSecretBox(key)
	if err != nil {
		return err
	}
	n, err := node.LoadIdentity(ctx, store, cfg.NodeName)
	if err != nil {
		return err
	}
	log = log.With("node_id", n.Identity.ID)
	auditLog := audit.New(store, log)
	incMgr := incidents.NewManager(store, log)
	prober := monitoring.NewProber(node.Version)
	engine := &monitoring.Engine{Store: store, Prober: prober, Incidents: incMgr, Log: log, NodeID: n.Identity.ID, Decrypt: api.DecryptMonitor(box),
		IsEvaluator: n.CanManage, Workers: cfg.ProbeWorkers, Retention: retention(store)}

	setupToken := ""
	if cnt, err := store.CountUsers(ctx); err != nil {
		return err
	} else if cnt == 0 {
		setupToken = cfg.SetupToken
		if setupToken == "" {
			setupToken = security.RandomToken(18)
		}
		log.Warn("initial setup required: open the web UI and enter this one-time setup token", "setup_token", setupToken)
	}
	srv := api.New(api.Deps{Config: cfg, Store: store, Node: n, Engine: engine, Prober: prober, Incidents: incMgr, Audit: auditLog,
		SecretBox: box, Log: log, Web: web.Handler(), SetupToken: setupToken})

	go engine.Run(ctx)
	go housekeeping(ctx, store, log)

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	errCh := make(chan error, 1)
	go func() {
		log.Info("intrabin listening", "addr", cfg.Listen, "version", node.Version, "role", n.Role(), "tls", cfg.TLSCert != "")
		if cfg.TLSCert != "" {
			httpSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			errCh <- httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shCtx)
}

func retention(store *storage.Store) func(ctx context.Context) (time.Duration, time.Duration, time.Duration) {
	return func(ctx context.Context) (time.Duration, time.Duration, time.Duration) {
		get := func(k string, def int) time.Duration {
			v, err := store.GetSetting(ctx, k)
			n, err2 := strconv.Atoi(v)
			if err != nil || err2 != nil || n <= 0 {
				n = def
			}
			return time.Duration(n) * 24 * time.Hour
		}
		return get("retention.raw_days", 14), get("retention.rounds_days", 90), get("retention.hourly_days", 400)
	}
}

func housekeeping(ctx context.Context, store *storage.Store, log *slog.Logger) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.DeleteExpiredSessions(ctx, time.Now(), 12*time.Hour); err != nil {
				log.Error("session cleanup", "err", err)
			} else if n > 0 {
				log.Info("expired sessions removed", "count", n)
			}
		}
	}
}

func healthcheck() int {
	addr := os.Getenv("INTRABIN_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if os.Getenv("INTRABIN_TLS_CERT") != "" {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- local liveness probe only
	}
	resp, err := client.Get(scheme + "://" + addr + "/api/v1/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}

// resetPassword is the offline credential recovery procedure (requires access to the data volume).
func resetPassword(args []string) int {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	user := fs.String("user", "", "username")
	_ = fs.Parse(args)
	if *user == "" {
		fmt.Fprintln(os.Stderr, "usage: intrabin reset-password -user <username>  (new password read from INTRABIN_NEW_PASSWORD or stdin)")
		return 2
	}
	pw := os.Getenv("INTRABIN_NEW_PASSWORD")
	if pw == "" {
		fmt.Fprint(os.Stderr, "New password: ")
		var line string
		if _, err := fmt.Scanln(&line); err != nil {
			fmt.Fprintln(os.Stderr, "read password:", err)
			return 1
		}
		pw = line
	}
	if err := security.ValidatePassword(pw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, cfg.DataDir+"/intrabin.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer store.Close()
	u, err := store.GetUserByUsername(ctx, *user)
	if err != nil {
		fmt.Fprintln(os.Stderr, "user not found")
		return 1
	}
	hash, err := security.HashPassword(pw, security.DefaultArgon2)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := store.SetPassword(ctx, u.ID, hash); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if u.Disabled {
		u.Disabled = false
		_ = store.UpdateUser(ctx, u)
	}
	audit.New(store, slog.Default()).Record(ctx, audit.Actor{Name: "cli"}, "user.password_reset_cli", "user", u.ID, "", nil)
	fmt.Println("password reset; all sessions of the user were revoked")
	return 0
}

func backup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("out", "", "destination file (must not exist)")
	_ = fs.Parse(args)
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: intrabin backup -out /data/backups/intrabin-YYYYMMDD.db")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, cfg.DataDir+"/intrabin.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer store.Close()
	if err := store.Backup(ctx, *out); err != nil {
		fmt.Fprintln(os.Stderr, "backup failed:", err)
		return 1
	}
	fmt.Println("backup written to", *out, "- also back up master.key (or INTRABIN_MASTER_KEY) separately")
	return 0
}

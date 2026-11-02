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
	"github.com/intrabin/intrabin/internal/automation"
	"github.com/intrabin/intrabin/internal/cluster"
	"github.com/intrabin/intrabin/internal/config"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/monitoring"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/runner"
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
		case "runner":
			os.Exit(runRunner())
		case "runner-exec":
			os.Exit(runner.ExecWrapper(os.Args[2:]))
		case "serve":
		default:
			fmt.Fprintf(os.Stderr, "usage: intrabin [serve|runner|healthcheck|version|reset-password|backup]\n")
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
	box, err := security.NewSecretBox(key) // data key: switched to the cluster data key in cluster mode
	if err != nil {
		return err
	}
	masterBox, err := security.NewSecretBox(key) // node master key: wraps the cluster data key at rest
	if err != nil {
		return err
	}
	n, err := node.LoadIdentity(ctx, store, cfg.NodeName)
	if err != nil {
		return err
	}
	log = log.With("node_id", n.Identity.ID)
	cp := controlplane.New(store)
	auditLog := audit.New(cp, log)
	incMgr := incidents.NewManager(controlplane.IncidentStore{Store: store, CP: cp}, log)
	prober := monitoring.NewProber(node.Version)
	clusterMgr := cluster.New(cluster.Options{DataDir: cfg.DataDir, Listen: cfg.ClusterListen, Advertise: cfg.ClusterAdvertise, APIURL: cfg.PublicURL,
		Location: cfg.NodeLocation, Store: store, CP: cp, DataBox: box, MasterBox: masterBox, Node: n, Log: log})
	engine := &monitoring.Engine{Store: store, CP: cp, Prober: prober, Incidents: incMgr, Log: log, NodeID: n.Identity.ID, Decrypt: api.DecryptMonitor(box),
		IsEvaluator: n.CanManage, Workers: cfg.ProbeWorkers, Retention: retention(store), ResultSink: clusterMgr.ResultSink(), Location: clusterMgr.LocationOf}
	var runnerClient automation.Runner
	if cfg.RunnerURL != "" {
		runnerClient = &automation.RunnerClient{URL: cfg.RunnerURL, Token: cfg.RunnerToken}
	}
	auto := &automation.Engine{Store: store, CP: cp, Incidents: incMgr, Fence: clusterMgr.Fence, Active: n.CanManage, Box: box, Runner: runnerClient, Log: log.With("component", "automation")}
	incMgr.OnTransition(auto.OnIncidentTransition)
	cp.X.OnApplied(func(op string) {
		if op == "state.restore" || (strings.HasPrefix(op, "monitor.") && op != "monitor.round" && op != "monitor.state") {
			engine.Reload()
		}
		if op == "state.restore" || strings.HasPrefix(op, "workflow.") || op == "run.create" {
			auto.Kick()
		}
	})
	clusterMgr.OnRoleChange(func(node.Role) { auto.Kick() })
	if err := clusterMgr.Resume(ctx); err != nil {
		return fmt.Errorf("resume cluster: %w", err)
	}
	defer clusterMgr.Shutdown()

	setupToken := ""
	if cnt, err := store.CountUsers(ctx); err != nil {
		return err
	} else if cnt == 0 && cfg.JoinToken == "" {
		setupToken = cfg.SetupToken
		if setupToken == "" {
			setupToken = security.RandomToken(18)
		}
		log.Warn("initial setup required: open the web UI and enter this one-time setup token", "setup_token", setupToken)
	}
	srv := api.New(api.Deps{Config: cfg, Store: store, CP: cp, Cluster: clusterMgr, Node: n, Engine: engine, Prober: prober, Incidents: incMgr, Audit: auditLog,
		SecretBox: box, Log: log, Web: web.Handler(), SetupToken: setupToken, Automation: auto})

	go engine.Run(ctx)
	go auto.Run(ctx)
	go housekeeping(ctx, cp, n, log)
	go autoCluster(ctx, cfg, clusterMgr, log)

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

func housekeeping(ctx context.Context, cp *controlplane.CP, n *node.Node, log *slog.Logger) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !n.CanManage() {
				continue // sessions are replicated; the controller cleans them up
			}
			if err := cp.DeleteExpiredSessions(ctx, time.Now().UTC(), 12*time.Hour); err != nil {
				log.Error("session cleanup", "err", err)
			}
			if err := cp.PruneRuns(ctx, time.Now().UTC().Add(-90*24*time.Hour)); err != nil {
				log.Error("workflow run cleanup", "err", err)
			}
		}
	}
}

// autoCluster founds or joins a cluster at first start when configured through the environment
// (used by automated deployments such as the 3-node Compose environment).
func autoCluster(ctx context.Context, cfg *config.Config, m *cluster.Manager, log *slog.Logger) {
	if m.Enabled() {
		return
	}
	if cfg.ClusterInit {
		if err := m.Init(ctx, ""); err != nil {
			log.Error("automatic cluster initialisation failed", "err", err)
		} else {
			log.Info("cluster initialised")
		}
		return
	}
	if cfg.JoinToken == "" {
		return
	}
	for attempt := 1; ; attempt++ {
		res, err := m.Join(ctx, cfg.JoinToken, "", cluster.DataAction(cfg.JoinDataAction))
		if err == nil {
			log.Info("joined cluster", "cluster_id", res.ClusterID, "backup", res.BackupPath)
			return
		}
		if errors.Is(err, cluster.ErrDataPresent) || errors.Is(err, cluster.ErrBadRequest) || strings.Contains(err.Error(), "join rejected") {
			log.Error("automatic cluster join failed; not retrying", "err", err)
			return
		}
		log.Warn("automatic cluster join failed; retrying", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
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
	if mode, _ := store.GetSetting(ctx, "local.cluster_mode"); mode == "1" {
		fmt.Fprintln(os.Stderr, "this node is a cluster member: a local password change would diverge from the replicated state.\n"+
			"Ask another owner/admin to reset the password in the UI on the leader. If every owner is locked out, see docs/operations/cluster.md (recovery).")
		return 1
	}
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

// runRunner starts the isolated Action Runner (`intrabin runner`). It is meant to run as its own
// container or service account, separate from the API/control-plane process.
func runRunner() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 1
	}
	log := newLogger(cfg).With("component", "runner")
	tok, err := runnerToken()
	if err != nil {
		log.Error("runner token", "err", err)
		return 1
	}
	maxTimeout := 10 * time.Minute
	if v := os.Getenv("INTRABIN_RUNNER_MAX_TIMEOUT"); v != "" {
		if maxTimeout, err = time.ParseDuration(v); err != nil {
			log.Error("INTRABIN_RUNNER_MAX_TIMEOUT", "err", err)
			return 1
		}
	}
	srv, err := runner.New(runner.Config{Token: tok, WorkDir: os.Getenv("INTRABIN_RUNNER_WORKDIR"), MaxTimeout: maxTimeout, Log: log})
	if err != nil {
		log.Error("runner", "err", err)
		return 1
	}
	listen := os.Getenv("INTRABIN_RUNNER_LISTEN")
	if listen == "" {
		listen = ":8090"
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sh)
	}()
	log.Info("action runner listening", "addr", listen, "tls", cfg.TLSCert != "")
	if cfg.TLSCert != "" {
		err = hs.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = hs.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("runner stopped", "err", err)
		return 1
	}
	return 0
}

func runnerToken() (string, error) {
	if f := os.Getenv("INTRABIN_RUNNER_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv("INTRABIN_RUNNER_TOKEN"), nil
}

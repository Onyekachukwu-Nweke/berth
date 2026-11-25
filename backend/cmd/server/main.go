// Command server runs mountabo's local API: it composes the GitHub connection
// flow and serves it over a loopback HTTP listener.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"

	httpadapter "github.com/goodylili/mountabo/internal/adapter/http"
	"github.com/goodylili/mountabo/internal/adapter/repository"
	"github.com/goodylili/mountabo/internal/ai"
	"github.com/goodylili/mountabo/internal/config"
	"github.com/goodylili/mountabo/internal/github"
	"github.com/goodylili/mountabo/internal/ssh"
	"github.com/goodylili/mountabo/internal/usecase"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger := slog.Default()

	// All state lives in Postgres: servers, deployments and their release log,
	// and the vault that seals the platform's own credentials (server SSH keys,
	// deploy keys, users' GitHub tokens) under MOUNTABO_MASTER_KEY.
	db, err := repository.OpenPostgres(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	keyStore, err := repository.NewVaultSQL(db, cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("open vault: %w", err)
	}

	// Stores. Everything below reads and writes through these.
	sshClient := ssh.NewClient()
	serverStore := repository.NewServerSQL(db)
	deploymentStore := repository.NewDeploymentSQL(db)
	accessStore := repository.NewAccessSQL(db)

	// GitHub, two ways. As the App: repo operations (deploy keys, Actions
	// secrets, workflow commits) run on an installation token for the account
	// that owns the repo. As the signed-in user: identity, and which repos they
	// can reach, so GitHub stays the source of truth for repo access.
	ghClient := github.NewClient()
	app, err := github.NewApp(cfg.GitHub.AppID, cfg.GitHub.AppSlug, cfg.GitHub.AppPrivateKey)
	if err != nil {
		return fmt.Errorf("github app: %w", err)
	}
	oauth := github.NewOAuth(cfg.GitHub.ClientID, cfg.GitHub.ClientSecret)

	// Who may do what: sessions resolve to users; access combines their GitHub
	// repo permissions with the placement policy (clients, servers, rules).
	authSvc := usecase.NewAuthService(oauth, ghClient, accessStore, accessStore, accessStore, keyStore, cfg.GitHub.Admins, logger)
	accessSvc := usecase.NewAccessService(accessStore, serverStore, accessStore, deploymentStore)
	guard := httpadapter.NewGuard(authSvc, accessSvc, logger)
	authHandler := httpadapter.NewAuthHandler(authSvc, accessSvc, cfg.GitHub.ClientID, cfg.GitHub.AppSlug, logger)
	adminHandler := httpadapter.NewAdminHandler(accessSvc, authSvc, app, logger)

	// Repo reads: the person's repo list uses their own token; reads inside one
	// repo (ports, tree, .env.example, run steps, branches) use the App.
	connector := usecase.NewGitHubConnector(ghClient, ghClient, app, authSvc)
	treeSvc := usecase.NewTreeService(app, ghClient)
	envExampleSvc := usecase.NewEnvExampleService(app, ghClient)
	runStepsSvc := usecase.NewRunStepsService(app, ghClient)
	branchesSvc := usecase.NewBranchesService(app, ghClient)
	githubHandler := httpadapter.NewGitHubHandler(connector, treeSvc, envExampleSvc, runStepsSvc, branchesSvc, usecase.NewRepoFileService(app, ghClient), logger)

	// Compose the server flow: SSH probe + bootstrap + key generation (all ssh),
	// JSON-file persistence, and vault secrets. One ssh.Client serves probe,
	// bootstrap, keygen, and port inspection.
	serverSvc := usecase.NewServerService(serverStore, sshClient, sshClient, sshClient, sshClient, sshClient, sshClient, keyStore)
	serverPortSvc := usecase.NewServerPortService(serverStore, keyStore, sshClient)
	serverMetricsSvc := usecase.NewServerMetricsService(serverStore, keyStore, sshClient)
	serverLogsSvc := usecase.NewServerLogsService(serverStore, keyStore, sshClient)
	// Uptime Kuma has no public HTTP setup endpoint, so mountabo generates a
	// fresh admin credential pair, seeds it into UK's SQLite from inside the
	// container, and persists it in the vault so the dashboard panel can
	// surface it the next time the page loads.
	uptimeKumaAdminSvc := usecase.NewUptimeKumaAdminService(serverStore, keyStore, sshClient)
	serversHandler := httpadapter.NewServersHandler(serverSvc, serverPortSvc, serverMetricsSvc, serverLogsSvc, uptimeKumaAdminSvc, accessSvc, logger)

	// Compose the deploy flow: commit the workflow + deploy.sh and provision the
	// environment/secrets (all github), reading the server record (JSON store)
	// and mountabo's stored key (vault). One github.Client and vault.Store
	// already in hand serve these too.
	//
	// Each deploy is tracked in an append-only event log, not just upserted away.
	// Shared in-process deploy lock: the deploy path acquires it to serialize
	// deploys per target; the monitor reads it to surface lock state and clears it
	// for the "clear stuck lock" recovery action.
	deployLocker := usecase.NewDeployLocker()
	// The deploy flow also mints a per-repo read-only deploy key: ssh generates
	// the keypair and installs the private half on the server; github registers
	// the public half on the repo. ghClient also dispatches workflow_dispatch so a
	// secrets change can redeploy. Constructed before the secrets service, which
	// re-applies deployments through it (as a DeployReapplier) on secret changes.
	deploySvc := usecase.NewDeployService(serverStore, keyStore, app, ghClient, ghClient, ghClient, deploymentStore, ghClient, ghClient, ghClient, deployLocker)
	deployHandler := httpadapter.NewDeployHandler(deploySvc, logger)

	// The secrets service manages an environment's GitHub Actions secrets: GitHub
	// is the single store — mountabo lists the configured names (+ updated times),
	// sets values (write-only), and deletes secrets, but never reads a value back.
	// On a change it re-applies the deployment (via deploySvc) so the running
	// container picks it up.
	secretsSvc := usecase.NewSecretsService(app, deploymentStore, ghClient, ghClient, ghClient, ghClient, deploySvc)
	secretsHandler := httpadapter.NewSecretsHandler(secretsSvc, logger)

	// Compose the monitor: configured deployments (SQLite store) enriched with
	// their GitHub Actions runs (github), read with the vault token, and the
	// server store so each deployment can surface its live app URL. Deleting a
	// deployment is a real teardown: ssh stops and removes the app's container(s)
	// on its server (vault key), github removes the committed deploy workflow +
	// deploy.sh from the repo (vault token), and the SQLite store then forgets
	// its tracking. Teardown is best-effort and logged.
	monitorSvc := usecase.NewMonitorService(deploymentStore, deploymentStore, deploymentStore, app, ghClient, serverStore, keyStore, sshClient, ghClient, ghClient, ghClient, deploymentStore, deployLocker, logger)
	// Rollback and redeploy run SSH-direct rather than through Actions: recovery
	// should not depend on GitHub being reachable and a runner being free, which
	// is exactly when you least want another moving part.
	rollbackSvc := usecase.NewRollbackService(deploymentStore, serverStore, keyStore, app, ghClient, sshClient, sshClient, deployLocker, logger)
	// App health: probe whether a deployed app is responding by curling it from
	// its own server over SSH (the same ssh.Client + server store + vault key
	// the metrics/logs services use), so the card shows real up/down status.
	appHealthSvc := usecase.NewAppHealthService(deploymentStore, serverStore, keyStore, sshClient)
	monitorHandler := httpadapter.NewMonitorHandler(monitorSvc, appHealthSvc, accessSvc, logger)
	releaseHandler := httpadapter.NewReleaseHandler(rollbackSvc, logger)

	// Compose the terminal page: run a single operator command on a set-up server
	// over SSH (the same ssh.Client + server store + vault key the read-only
	// metrics/logs services use), and an AI command helper backed by Anthropic.
	// The AI client reads ANTHROPIC_API_KEY from config; an empty key makes the
	// helper return a structured "not configured" result instead of failing. The
	// helper only suggests, the operator runs the command through /exec.
	serverExecSvc := usecase.NewServerExecService(serverStore, keyStore, sshClient)
	aiClient := ai.NewClient(cfg.AI.APIKey, cfg.AI.Model)
	aiCommandSvc := usecase.NewAICommandService(aiClient)
	terminalHandler := httpadapter.NewTerminalHandler(serverExecSvc, aiCommandSvc, logger)

	router := httpadapter.NewRouter(guard, authHandler, adminHandler, githubHandler, serversHandler, deployHandler, monitorHandler, secretsHandler, releaseHandler, terminalHandler)
	srv := httpadapter.NewServer(cfg, router)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Onyekachukwu-Nweke/berth/internal/workflow"
)

// secretName matches a valid GitHub Actions secret name: letters, digits and
// underscores, not starting with a digit. Env var keys must satisfy this before
// their values can be stored as secrets.
var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envNameSafe matches a path-safe environment name: letters, digits, dot, dash,
// underscore. The environment is a URL path segment in the deployment API, so it
// must not contain slashes or spaces.
var envNameSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ErrDeployInProgress is returned when a deploy to a target is rejected because
// another deploy to the same target (app) is already in flight.
var ErrDeployInProgress = errors.New("deploy already in progress for this target")

// ErrLegacyDeployment is returned by ReapplyWorkflow for a deployment recorded
// before its deploy config was persisted: berth can't regenerate its artifacts,
// so a secrets-tab change can't be auto-applied — a full redeploy is needed.
var ErrLegacyDeployment = errors.New("deployment predates persisted config; redeploy from configure to apply")

// DeployPort is one published port the operator configured. EnvVar/Value back a
// compose stack's host port; Value/Container back a `docker run -p` mapping.
type DeployPort struct {
	EnvVar    string
	Value     string
	Container string
}

// DeployEnvVar is one application environment variable; its value is stored as a
// GitHub Actions secret and injected into the deploy at run time.
type DeployEnvVar struct {
	Key   string
	Value string
}

// DeployConfig is the non-secret deploy configuration persisted with a
// deployment so a re-apply can regenerate the workflow + deploy.sh without the
// operator re-entering it (env-var values are never here — they live only as
// GitHub secrets). RootDir/App/Branch/etc. are already columns on the record.
type DeployConfig struct {
	Strategy      string       `json:"strategy"`
	DeployDir     string       `json:"deployDir"`
	DeployKeyFile string       `json:"deployKeyFile"`
	Ports         []DeployPort `json:"ports"`
	// Hooks are the operator's release hooks. Persisted here because they are baked
	// into the committed script, so ReapplyWorkflow must regenerate it from this
	// record alone.
	Hooks []Hook `json:"hooks,omitempty"`
	// ComposeFile is an explicit compose file path for stacks that do not use one
	// of the conventional names (docker-compose.prod.yml being the common case).
	// Empty falls back to those names. Persisted because ReapplyWorkflow
	// regenerates the script from this record alone — GitHub cannot tell it which
	// compose file a stack uses.
	ComposeFile string `json:"composeFile,omitempty"`
}

// DeployInput is what the operator supplies to wire continuous deployment of a
// repo branch to one of their servers. Strategy is "compose" or "docker"
// (defaulting to compose); Environment names the GitHub deployment environment
// whose secrets the workflow uses, defaulting to Branch.
type DeployInput struct {
	ServerID    string
	App         string
	Owner       string
	Repo        string
	Branch      string
	Environment string
	Strategy    string
	RootDir     string
	DeployDir   string
	ComposeFile string
	Hooks       []Hook
	Ports       []DeployPort
	EnvVars     []DeployEnvVar
}

// NamedSecret is a GitHub Actions secret to set on an environment. Values are
// never logged or echoed; only names appear in progress output.
type NamedSecret struct {
	Name  string
	Value string
}

// SecretMeta describes a secret the deploy needs, without its value: Managed is
// true for the server-connection secrets berth fills in, false for the
// operator's own env vars. Used by the preview so the UI can show what will be
// set and by whom.
type SecretMeta struct {
	Name    string `json:"name"`
	Managed bool   `json:"managed"`
}

// DeployArtifacts is exactly what berth will commit and configure: the
// workflow file (and its path), deploy.sh, and the secrets it needs. Generated
// purely from a DeployInput so the UI can preview byte-for-byte what a deploy
// would do.
type DeployArtifacts struct {
	WorkflowPath string       `json:"workflowPath"`
	Workflow     string       `json:"workflow"`
	DeployScript string       `json:"deployScript"`
	Secrets      []SecretMeta `json:"secrets"`
}

// ── ports (consumed here, implemented by the github adapter) ──

// RepoWriter commits a single file to a repo branch via the contents API,
// creating it or updating it in place.
type RepoWriter interface {
	PutFile(ctx context.Context, t Token, owner, repo, branch, path, content, message string) error
}

// EnvManager creates (or leaves in place) a deployment environment on a repo.
type EnvManager interface {
	EnsureEnvironment(ctx context.Context, t Token, owner, repo, name string) error
}

// EnvSecretSetter stores Actions secrets on a repo's deployment environment,
// encrypting each value with the environment's public key.
type EnvSecretSetter interface {
	SetEnvSecrets(ctx context.Context, t Token, owner, repo, environment string, secrets []NamedSecret) error
}

// RefResolver reports the head commit SHA of a repo branch, so a deploy can
// record the exact commit it is releasing. Best-effort at deploy time: a failure
// leaves the release's commit unset rather than blocking the deploy.
type RefResolver interface {
	HeadSHA(ctx context.Context, t Token, owner, repo, branch string) (string, error)
}

// WorkflowDispatcher triggers a workflow run via GitHub's workflow_dispatch event
// (by workflow file name, on a ref), so a re-apply can redeploy without waiting
// for a push. Used when a secret changes so the running container picks it up.
type WorkflowDispatcher interface {
	Dispatch(ctx context.Context, t Token, owner, repo, workflowFile, ref string) error
}

// DeployService configures continuous deployment of a repo to a server: it
// commits the workflow and deploy.sh, creates the deployment environment, and
// stores the secrets the workflow needs (server connection details, managed by
// berth, plus the operator's own env vars). It is also the single source of
// truth for the generated files, so the UI previews via Preview.
type DeployService struct {
	servers    ServerStore
	vault      SecretVault
	tokens     RepoTokens
	repo       RepoWriter
	envs       EnvManager
	secrets    EnvSecretSetter
	history    DeploymentStore
	repoKeys   RepoKeyDeleter
	refs       RefResolver
	dispatcher WorkflowDispatcher
	locker     *DeployLocker
}

var _ DeployReapplier = (*DeployService)(nil)

// NewDeployService wires the service to its ports. refs enriches the recorded
// release with the deployed commit (the actor is the signed-in user, taken from
// the request context); it is read best-effort and never block a deploy. dispatcher redeploys via
// workflow_dispatch when a secret change is re-applied. locker serializes deploys
// per target so two concurrent deploys never race on the same repo files.
func NewDeployService(servers ServerStore, vault SecretVault, tokens RepoTokens, repo RepoWriter, envs EnvManager, secrets EnvSecretSetter, history DeploymentStore, repoKeys RepoKeyDeleter, refs RefResolver, dispatcher WorkflowDispatcher, locker *DeployLocker) *DeployService {
	return &DeployService{servers: servers, vault: vault, tokens: tokens, repo: repo, envs: envs, secrets: secrets, history: history, repoKeys: repoKeys, refs: refs, dispatcher: dispatcher, locker: locker}
}

// Preview generates the deploy artifacts from the config alone, no server, no
// token, no side effects, so the configure UI can show exactly what a deploy
// would commit and set.
func (s *DeployService) Preview(in DeployInput) (DeployArtifacts, error) {
	if err := validateDeploy(in); err != nil {
		return DeployArtifacts{}, err
	}
	cfg := buildConfig(in)
	return DeployArtifacts{
		WorkflowPath: workflow.Path(cfg),
		Workflow:     workflow.Workflow(cfg),
		DeployScript: workflow.DeployScript(cfg),
		Secrets:      secretMetas(in),
	}, nil
}

// Deploy writes the deploy artifacts to the repo, provisions the environment,
// and sets its secrets, streaming each step to out so the operator follows
// along live. The deployment itself runs later on GitHub's runner (triggered by
// a push), so nothing executes on the server here; what runs there is the
// committed deploy.sh, observable in the Actions log. SERVER_SSH_KEY is
// berth's stored private key for the box; it is set as a secret but never
// written to out.
func (s *DeployService) Deploy(ctx context.Context, in DeployInput, out io.Writer) error {
	if in.ServerID == "" {
		return fmt.Errorf("server is required")
	}
	if err := validateDeploy(in); err != nil {
		return err
	}

	server, err := s.servers.Get(in.ServerID)
	if err != nil {
		return err
	}
	if server.Status != StatusReady {
		return fmt.Errorf("server must be set up before deploying")
	}

	// The environment is the deployment's identity; it defaults to the branch when
	// the operator leaves it blank (back-compat with pre-environment deploys).
	env := strings.TrimSpace(in.Environment)
	if env == "" {
		env = strings.TrimSpace(in.Branch)
	}

	// Serialize deploys to the same target (app within an environment) so two
	// concurrent runs (a double click, two tabs, a reconnecting client) never race
	// on the same repo files and secrets. Held only for this local operation;
	// released on return, so a client disconnect (ctx cancel) also frees it. A
	// wedged deploy is recovered via the "clear stuck lock" action.
	if !s.locker.TryAcquire(lockKey(in.Owner, in.Repo, env)) {
		return ErrDeployInProgress
	}
	defer s.locker.Release(lockKey(in.Owner, in.Repo, env))

	token, err := s.tokens.TokenFor(ctx, in.Owner)
	if err != nil {
		return fmt.Errorf("load github token: %w", err)
	}

	// berth's private key for this box becomes SERVER_SSH_KEY so GitHub's
	// runner can SSH in as the berth user (its public half is already an
	// authorized_key from bootstrap).
	key, err := s.vault.LoadSecret(privateKeyKey(in.ServerID))
	if err != nil {
		return fmt.Errorf("load server key: %w", err)
	}

	// Use the validated set: it is in canonical phase order and has the production
	// migration gate forced on, neither of which the caller can be trusted to do.
	if hooks, herr := ValidateHooks(in.Hooks, in.Strategy, env); herr == nil {
		in.Hooks = hooks
	}
	cfg := buildConfig(in)
	// An already-configured deployment keeps the identity its live containers,
	// images and paths are named from. buildConfig only derives one because
	// Preview has no record to read.
	if existing, found := s.findDeployment(in.Owner, in.Repo, env); found {
		cfg.DeploymentID = DeploymentIDFor(existing)
	}
	// Nothing is cloned onto the server any more: the runner ships the compose
	// files with the images. Servers deployed by older versions still hold a
	// read-only deploy key for the repo; naming it lets deploy.sh delete it.
	if cfg.Strategy != workflow.Docker {
		cfg.DeployKeyFile = deployKeyFileName(in.Owner, in.Repo)
	}

	// Everything the run needs goes in place BEFORE the workflow is committed:
	// that commit triggers a run straight away, and GitHub hands a job its
	// environment secrets when the job starts. Committing first meant the first
	// run started with empty SERVER_HOST/SERVER_USER and failed at its SSH step.
	progress(out, "creating environment %s", env)
	if err := s.envs.EnsureEnvironment(ctx, token, in.Owner, in.Repo, env); err != nil {
		return fmt.Errorf("create environment: %w", err)
	}

	secrets := deploySecrets(server, key, in)
	progress(out, "setting %d secrets on environment %s", len(secrets), env)
	if err := s.secrets.SetEnvSecrets(ctx, token, in.Owner, in.Repo, env, secrets); err != nil {
		return fmt.Errorf("set environment secrets: %w", err)
	}

	s.retireDeployKey(ctx, token, in.Owner, in.Repo, env, out)

	scriptPath := workflow.ScriptPath(cfg)
	progress(out, "writing %s", scriptPath)
	if err := s.repo.PutFile(ctx, token, in.Owner, in.Repo, in.Branch, scriptPath, workflow.DeployScript(cfg), "berth: add deploy script"); err != nil {
		return fmt.Errorf("write deploy script: %w", err)
	}

	wfPath := workflow.Path(cfg)
	progress(out, "writing %s", wfPath)
	if err := s.repo.PutFile(ctx, token, in.Owner, in.Repo, in.Branch, wfPath, workflow.Workflow(cfg), "berth: add deploy workflow"); err != nil {
		return fmt.Errorf("write workflow: %w", err)
	}

	// Resolve the release's commit and actor for the durable record. Both are
	// best-effort: a failure leaves the field empty and only notes it, since the
	// deploy has already succeeded and must not fail on tracking metadata.
	commitSHA, err := s.refs.HeadSHA(ctx, token, in.Owner, in.Repo, in.Branch)
	if err != nil {
		progress(out, "note: could not resolve commit for %s (release will omit it): %v", in.Branch, err)
	}
	// The actor is the signed-in person who asked for the deploy.
	var actor string
	if u, ok := UserFrom(ctx); ok {
		actor = u.Login
	}

	// Record the deployment so the monitor can show its history. A failure here
	// must not fail the deploy, which already succeeded, so it is only noted.
	record := Deployment{
		ID:           newID(),
		App:          cfg.App,
		Owner:        in.Owner,
		Repo:         in.Repo,
		Branch:       in.Branch,
		Environment:  env,
		ServerID:     in.ServerID,
		WorkflowFile: workflow.FileName(env),
		// The canonical identity every server-side resource is named from. Save
		// writes it only on first insert, so an existing deployment keeps the id
		// its live containers, images and paths already carry.
		DeploymentID: DeriveDeploymentID(in.Owner, in.Repo, env),
		Port:         primaryPort(in.Ports),
		RootDir:      in.RootDir,
		Config: &DeployConfig{
			Strategy:      in.Strategy,
			DeployDir:     strings.TrimSpace(in.DeployDir),
			DeployKeyFile: cfg.DeployKeyFile,
			Ports:         in.Ports,
			ComposeFile:   strings.TrimSpace(in.ComposeFile),
			Hooks:         in.Hooks,
		},
		CreatedAt: time.Now().UTC(),
		CommitSHA: commitSHA,
		Actor:     actor,
	}
	if err := s.history.Save(record); err != nil {
		progress(out, "note: deploy succeeded but recording history failed: %v", err)
	}

	// Committing the workflow file to in.Branch above fires its `on: push`
	// trigger, which runs the deploy — so no explicit workflow_dispatch is needed
	// (that would produce a second, duplicate run). workflow_dispatch stays in the
	// generated workflow for manual re-runs from GitHub.
	progress(out, "deploy configured, watch the %s run on %s", cfg.Strategy, in.Branch)
	return nil
}

// primaryPort is the app's main published host port, taken from the first
// configured deploy port whose Value parses as a port number. It is recorded on
// the deployment so a health check can probe 127.0.0.1:<port> on the server. 0
// when no usable port is configured.
func primaryPort(ports []DeployPort) int {
	for _, p := range ports {
		if n, err := strconv.Atoi(strings.TrimSpace(p.Value)); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// validateDeploy checks the config fields common to preview and deploy.
func validateDeploy(in DeployInput) error {
	if strings.TrimSpace(in.App) == "" || strings.TrimSpace(in.Owner) == "" ||
		strings.TrimSpace(in.Repo) == "" || strings.TrimSpace(in.Branch) == "" {
		return fmt.Errorf("app, owner, repo and branch are required")
	}
	if strings.TrimSpace(in.DeployDir) == "" {
		return fmt.Errorf("deploy directory is required")
	}
	// The environment names the deployment and becomes a URL path segment in the
	// deployment API (/api/deployments/{owner}/{repo}/{environment}/...), so it
	// must be path-safe. It defaults to the branch, which can contain a slash
	// (feature/foo) — reject that here rather than storing a record that the
	// per-environment routes can't address.
	env := strings.TrimSpace(in.Environment)
	if env == "" {
		env = strings.TrimSpace(in.Branch)
	}
	if !envNameSafe.MatchString(env) {
		return fmt.Errorf("environment name %q must be letters, numbers, dots, dashes or underscores (no slashes or spaces); name the environment explicitly if your branch contains a slash", env)
	}
	for _, v := range in.EnvVars {
		if k := strings.TrimSpace(v.Key); k != "" && !secretName.MatchString(k) {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
	}
	// Reject an unrunnable hook here rather than committing it into the operator's
	// repository and failing on the server, where the error is far harder to read.
	if _, err := ValidateHooks(in.Hooks, in.Strategy, env); err != nil {
		return err
	}
	return nil
}

// ReapplyWorkflow regenerates a deployment's committed workflow + deploy.sh from
// its persisted config and the given operator env-var names, then triggers a
// redeploy via workflow_dispatch so the running container picks up a secret
// change. The commit message carries [skip ci] so its push does not also trigger
// a run (the explicit dispatch is the single trigger); PutFile is idempotent, so
// a value-only edit (identical artifacts) makes no commit and just dispatches.
// A deployment with no persisted config (recorded before this existed) returns
// ErrLegacyDeployment — it must be redeployed from configure first.
func (s *DeployService) ReapplyWorkflow(ctx context.Context, t Token, owner, repo, environment string, envKeys []string) error {
	dep, found := s.findDeployment(owner, repo, environment)
	if !found {
		return ErrDeploymentNotFound
	}
	if dep.Config == nil {
		return ErrLegacyDeployment
	}

	ports := make([]workflow.Port, 0, len(dep.Config.Ports))
	for _, p := range dep.Config.Ports {
		ports = append(ports, workflow.Port{EnvVar: p.EnvVar, Value: p.Value, Container: p.Container})
	}
	envs := make([]workflow.EnvVar, 0, len(envKeys))
	for _, k := range envKeys {
		envs = append(envs, workflow.EnvVar{Key: k})
	}
	strategy := workflow.Compose
	if dep.Config.Strategy == string(workflow.Docker) {
		strategy = workflow.Docker
	}
	cfg := workflow.Config{
		App:           dep.App,
		Owner:         owner,
		Repo:          repo,
		Branch:        dep.Branch,
		Environment:   dep.Environment,
		RootDir:       dep.RootDir,
		DeployDir:     dep.Config.DeployDir,
		Strategy:      strategy,
		Ports:         ports,
		EnvVars:       envs,
		DeployKeyFile: dep.Config.DeployKeyFile,
		DeploymentID:  DeploymentIDFor(dep),
		ComposeFile:   dep.Config.ComposeFile,
		Hooks:         WorkflowHooks(dep.Config.Hooks),
		HooksVersion:  HooksVersion(dep.Config.Hooks),
	}

	const msg = "berth: sync deploy config for secrets change [skip ci]"
	if err := s.repo.PutFile(ctx, t, owner, repo, dep.Branch, workflow.ScriptPath(cfg), workflow.DeployScript(cfg), msg); err != nil {
		return fmt.Errorf("update deploy script: %w", err)
	}
	if err := s.repo.PutFile(ctx, t, owner, repo, dep.Branch, workflow.Path(cfg), workflow.Workflow(cfg), msg); err != nil {
		return fmt.Errorf("update workflow: %w", err)
	}
	if err := s.dispatcher.Dispatch(ctx, t, owner, repo, dep.WorkflowFile, dep.Branch); err != nil {
		return fmt.Errorf("dispatch redeploy: %w", err)
	}
	return nil
}

// findDeployment returns the tracked deployment for (owner, repo, environment).
func (s *DeployService) findDeployment(owner, repo, environment string) (Deployment, bool) {
	deployments, err := s.history.List()
	if err != nil {
		return Deployment{}, false
	}
	for _, d := range deployments {
		if d.Owner == owner && d.Repo == repo && d.Environment == environment {
			return d, true
		}
	}
	return Deployment{}, false
}

// buildConfig maps a validated DeployInput onto the generator's config.
func buildConfig(in DeployInput) workflow.Config {
	strategy := workflow.Compose
	if in.Strategy == string(workflow.Docker) {
		strategy = workflow.Docker
	}
	ports := make([]workflow.Port, 0, len(in.Ports))
	for _, p := range in.Ports {
		ports = append(ports, workflow.Port{EnvVar: p.EnvVar, Value: p.Value, Container: p.Container})
	}
	envs := make([]workflow.EnvVar, 0, len(in.EnvVars))
	for _, v := range in.EnvVars {
		envs = append(envs, workflow.EnvVar{Key: v.Key, Value: v.Value})
	}
	env := strings.TrimSpace(in.Environment)
	if env == "" {
		env = strings.TrimSpace(in.Branch)
	}
	return workflow.Config{
		App:         strings.TrimSpace(in.App),
		Owner:       strings.TrimSpace(in.Owner),
		Repo:        strings.TrimSpace(in.Repo),
		Branch:      strings.TrimSpace(in.Branch),
		Environment: strings.TrimSpace(in.Environment),
		// Derived, not read from a record: Preview has no stored deployment. The
		// Deploy path overrides this with the stored id when one exists.
		DeploymentID: DeriveDeploymentID(strings.TrimSpace(in.Owner), strings.TrimSpace(in.Repo), env),
		RootDir:      in.RootDir,
		DeployDir:    strings.TrimSpace(in.DeployDir),
		ComposeFile:  strings.TrimSpace(in.ComposeFile),
		Hooks:        WorkflowHooks(in.Hooks),
		HooksVersion: HooksVersion(in.Hooks),
		Strategy:     strategy,
		Ports:        ports,
		EnvVars:      envs,
	}
}

// deploySecrets is the full secret set with values: server connection details
// berth manages, the deploy directory, then the operator's env vars (blank
// keys dropped), in a stable order.
func deploySecrets(server Server, sshKey string, in DeployInput) []NamedSecret {
	secrets := []NamedSecret{
		{Name: "SERVER_HOST", Value: server.IP},
		{Name: "SERVER_USER", Value: BootstrapUser},
		{Name: "SERVER_SSH_KEY", Value: sshKey},
		{Name: "DEPLOY_DIR", Value: strings.TrimSpace(in.DeployDir)},
	}
	for _, v := range in.EnvVars {
		if k := strings.TrimSpace(v.Key); k != "" {
			secrets = append(secrets, NamedSecret{Name: k, Value: v.Value})
		}
	}
	return secrets
}

// secretMetas is the same secret set as deploySecrets but names-only, for the
// preview: SERVER_* + DEPLOY_DIR are mountabo-managed, env vars are the
// operator's.
func secretMetas(in DeployInput) []SecretMeta {
	metas := make([]SecretMeta, 0, len(managedSecretNames)+len(in.EnvVars))
	for _, name := range managedSecretNames {
		metas = append(metas, SecretMeta{Name: name, Managed: true})
	}
	for _, v := range in.EnvVars {
		if k := strings.TrimSpace(v.Key); k != "" {
			metas = append(metas, SecretMeta{Name: k, Managed: false})
		}
	}
	return metas
}

// managedSecretNames are the server-connection secrets berth sets itself
// (from the server + vault), in stable order. They are never editable, deletable
// or revealable through the secrets UI.
var managedSecretNames = []string{"SERVER_HOST", "SERVER_USER", "SERVER_SSH_KEY", "DEPLOY_DIR"}

// isManagedSecret reports whether a secret name is one berth manages itself.
func isManagedSecret(name string) bool {
	for _, m := range managedSecretNames {
		if m == name {
			return true
		}
	}
	return false
}

// deployKeyVaultKey is the vault entry holding a repo's deploy private key.
func deployKeyVaultKey(owner, repo string) string {
	return "deploykey-" + owner + "-" + repo
}

// deployKeyFileName is the (filesystem-safe) name the deploy key is installed
// under in the deploy user's ~/.ssh, one per repo.
// retireDeployKey removes the read-only deploy key that older deploys added to
// the repo (and kept in the vault) to clone it onto servers. It only does so
// when no other environment of the repo exists: one that hasn't been
// redeployed yet still runs a deploy.sh that clones with that key. Failure is
// only reported; it never fails the deploy.
func (s *DeployService) retireDeployKey(ctx context.Context, token Token, owner, repo, env string, out io.Writer) {
	if k, _ := s.vault.LoadSecret(deployKeyVaultKey(owner, repo)); k == "" {
		return
	}
	all, err := s.history.List()
	if err != nil {
		return
	}
	var others []string
	for _, d := range all {
		if strings.EqualFold(d.Owner, owner) && strings.EqualFold(d.Repo, repo) && !strings.EqualFold(d.Environment, env) {
			others = append(others, d.Environment)
		}
	}
	if len(others) > 0 {
		progress(out, "note: keeping %s/%s's old deploy key until %s %s redeployed", owner, repo, strings.Join(others, ", "), pluralIs(len(others)))
		return
	}
	if err := s.repoKeys.DeleteDeployKey(ctx, token, owner, repo, "mountabo deploy"); err != nil {
		progress(out, "note: could not remove the old deploy key from %s/%s: %v", owner, repo, err)
		return
	}
	_ = s.vault.DeleteSecret(deployKeyVaultKey(owner, repo))
	progress(out, "removed the old read-only deploy key from %s/%s (servers no longer clone the repository)", owner, repo)
}

func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func deployKeyFileName(owner, repo string) string {
	var b strings.Builder
	b.WriteString("mountabo_deploy_")
	for _, r := range owner + "_" + repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// progress writes one "==> ..." line to the live output stream, the same
// convention the bootstrap and apply-options flows use.
func progress(out io.Writer, format string, args ...any) {
	_, _ = io.WriteString(out, "==> "+fmt.Sprintf(format, args...)+"\n")
}

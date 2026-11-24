package repository

import (
	"testing"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
)

func TestDeploymentSQL_UpsertsStateAndTracksEvents(t *testing.T) {
	db := testDB(t)
	store := NewDeploymentSQL(db)

	if list, err := store.List(); err != nil || len(list) != 0 {
		t.Fatalf("fresh db: list=%v err=%v", list, err)
	}

	first := usecase.Deployment{
		ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "main", ServerID: "s1", WorkflowFile: "mountabo-deploy-main.yml",
		Port:      8080,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(first); err != nil {
		t.Fatalf("Save first: %v", err)
	}

	// Re-deploy the same target with changed fields: state upserts (still one
	// row), id + created_at stay original, other fields update.
	second := first
	second.ID = "ignored"
	second.ServerID = "s2"
	second.CreatedAt = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(second); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 deployment, got %d", len(list))
	}
	got := list[0]
	if got.ID != "d1" {
		t.Errorf("id = %q, want the original d1 kept on upsert", got.ID)
	}
	if got.ServerID != "s2" {
		t.Errorf("serverID = %q, want updated to s2", got.ServerID)
	}
	if got.Port != 8080 {
		t.Errorf("port = %d, want 8080 persisted", got.Port)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("createdAt = %v, want the original first-deploy time", got.CreatedAt)
	}

	// Both deploys are tracked in the append-only event log.
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deploy_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 2 {
		t.Errorf("deploy_events = %d, want 2", events)
	}

	// Deleting by identity removes the state row and its whole event history, and
	// reports that something was removed; deleting an unknown target reports false.
	removed, err := store.Delete("acme", "shop", "main")
	if err != nil || !removed {
		t.Fatalf("Delete(acme/shop/main): removed=%v err=%v", removed, err)
	}
	if list, err := store.List(); err != nil || len(list) != 0 {
		t.Fatalf("after delete: list=%v err=%v", list, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM deploy_events`).Scan(&events); err != nil {
		t.Fatalf("count events after delete: %v", err)
	}
	if events != 0 {
		t.Errorf("deploy_events after delete = %d, want 0", events)
	}
	if removed, err := store.Delete("acme", "shop", "nope"); err != nil || removed {
		t.Errorf("Delete(unknown env): removed=%v err=%v, want false/nil", removed, err)
	}
}

// TestDeploymentSQL_EnvironmentsCoexist proves two environments of the same repo
// (and even the same app name) are independent rows, keyed by environment: each
// upserts and deletes on its own without touching the other.
func TestDeploymentSQL_EnvironmentsCoexist(t *testing.T) {
	db := testDB(t)
	store := NewDeploymentSQL(db)

	prod := usecase.Deployment{
		ID: "p1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "production", ServerID: "s1", WorkflowFile: "mountabo-deploy-production.yml",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	staging := usecase.Deployment{
		ID: "s1id", App: "shop", Owner: "acme", Repo: "shop", Branch: "develop",
		Environment: "staging", ServerID: "s2", WorkflowFile: "mountabo-deploy-staging.yml",
		CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(prod); err != nil {
		t.Fatalf("Save production: %v", err)
	}
	if err := store.Save(staging); err != nil {
		t.Fatalf("Save staging: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 coexisting environments, got %d", len(list))
	}

	// Deleting staging leaves production intact.
	if removed, err := store.Delete("acme", "shop", "staging"); err != nil || !removed {
		t.Fatalf("Delete staging: removed=%v err=%v", removed, err)
	}
	list, _ = store.List()
	if len(list) != 1 || list[0].Environment != "production" {
		t.Fatalf("after deleting staging, want only production, got %+v", list)
	}
}

func TestDeploymentSQL_RecordsReleasesAndLinksPrevious(t *testing.T) {
	db := testDB(t)
	store := NewDeploymentSQL(db)

	base := usecase.Deployment{
		ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "main", ServerID: "s1", WorkflowFile: "mountabo-deploy-main.yml",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	first := base
	first.CommitSHA, first.Actor = "aaa111", "octocat"
	if err := store.Save(first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second := base
	second.CommitSHA, second.Actor = "bbb222", "hubot"
	if err := store.Save(second); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	releases, total, err := store.Releases("acme", "shop", "main", 10)
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if total != 2 || len(releases) != 2 {
		t.Fatalf("releases total=%d len=%d, want 2/2", total, len(releases))
	}
	// Newest first: the second deploy leads.
	newest, oldest := releases[0], releases[1]
	if newest.CommitSHA != "bbb222" || newest.Actor != "hubot" {
		t.Errorf("newest commit/actor = %q/%q, want bbb222/hubot", newest.CommitSHA, newest.Actor)
	}
	if newest.Status != "pending" {
		t.Errorf("newest status = %q, want pending", newest.Status)
	}
	// The newest release links back to the oldest; the first release links to nothing.
	if newest.PrevReleaseID == nil || *newest.PrevReleaseID != oldest.ID {
		t.Errorf("newest prevReleaseID = %v, want %d", newest.PrevReleaseID, oldest.ID)
	}
	if oldest.PrevReleaseID != nil {
		t.Errorf("oldest prevReleaseID = %v, want nil", *oldest.PrevReleaseID)
	}
}

// TestDeploymentSQL_PersistsDeployConfig proves the non-secret deploy config
// round-trips through the deployments row, and that a record saved without it
// reads back as legacy (Config nil).
func TestDeploymentSQL_PersistsDeployConfig(t *testing.T) {
	db := testDB(t)
	store := NewDeploymentSQL(db)

	withCfg := usecase.Deployment{
		ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "production", ServerID: "s1", WorkflowFile: "wf.yml",
		Config: &usecase.DeployConfig{
			Strategy: "compose", DeployDir: "/opt/shop", DeployKeyFile: "k",
			Ports: []usecase.DeployPort{{EnvVar: "FRONTEND_PORT", Value: "3000", Container: "3000"}},
		},
	}
	legacy := usecase.Deployment{
		ID: "d2", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "staging", ServerID: "s1", WorkflowFile: "wf.yml",
	}
	if err := store.Save(withCfg); err != nil {
		t.Fatalf("Save withCfg: %v", err)
	}
	if err := store.Save(legacy); err != nil {
		t.Fatalf("Save legacy: %v", err)
	}

	byEnv := map[string]usecase.Deployment{}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, d := range list {
		byEnv[d.Environment] = d
	}

	got := byEnv["production"].Config
	if got == nil {
		t.Fatal("production should have persisted config")
	}
	if got.Strategy != "compose" || got.DeployDir != "/opt/shop" || got.DeployKeyFile != "k" ||
		len(got.Ports) != 1 || got.Ports[0].EnvVar != "FRONTEND_PORT" || got.Ports[0].Value != "3000" {
		t.Errorf("config round-trip wrong: %+v", got)
	}
	if byEnv["staging"].Config != nil {
		t.Errorf("a record saved without config should read back legacy (nil), got %+v", byEnv["staging"].Config)
	}
}

// TestDeploymentSQL_PersistsDeploymentIDOnceAndNeverMovesIt pins the "written
// once" rule. The stored id names the containers, images, volumes and paths that
// already exist on the server, so a later deploy that renames the app or moves
// the branch must not be able to re-home them.
func TestDeploymentSQL_PersistsDeploymentIDOnceAndNeverMovesIt(t *testing.T) {
	db := testDB(t)
	store := NewDeploymentSQL(db)

	d := usecase.Deployment{
		ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "production", ServerID: "s1", WorkflowFile: "w.yml",
		DeploymentID: usecase.DeriveDeploymentID("acme", "shop", "production"),
		CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	moved := d
	moved.App = "renamed"
	moved.Branch = "other"
	moved.DeploymentID = "mtb-000000000000"
	if err := store.Save(moved); err != nil {
		t.Fatalf("Save again: %v", err)
	}

	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v err=%v", list, err)
	}
	if got := list[0].DeploymentID; got != d.DeploymentID {
		t.Errorf("deployment id moved to %q, want the original %q", got, d.DeploymentID)
	}
	if list[0].App != "renamed" || list[0].Branch != "other" {
		t.Errorf("other fields should still update: app=%q branch=%q", list[0].App, list[0].Branch)
	}
}

// TestDeploymentSQL_EnvironmentsGetDistinctIdentities is the storage half of
// acceptance criterion 3. Two environments of one repo previously shared
// "<app>-<branch>" for their compose project, containers and image namespace;
// their identities must now differ at the point they are recorded.
func TestDeploymentSQL_EnvironmentsGetDistinctIdentities(t *testing.T) {
	db := testDB(t)
	store := NewDeploymentSQL(db)

	// Same repo, same branch, two environments — the exact collision case.
	for _, env := range []string{"production", "staging"} {
		if err := store.Save(usecase.Deployment{
			ID: "d-" + env, App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
			Environment: env, ServerID: "s1", WorkflowFile: "w.yml",
			DeploymentID: usecase.DeriveDeploymentID("acme", "shop", env),
		}); err != nil {
			t.Fatalf("Save %s: %v", env, err)
		}
	}

	list, err := store.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %v err=%v", list, err)
	}
	if list[0].DeploymentID == list[1].DeploymentID {
		t.Errorf("both environments share the identity %q", list[0].DeploymentID)
	}
	for _, d := range list {
		if d.DeploymentID == "" {
			t.Errorf("environment %q has no identity", d.Environment)
		}
	}
}

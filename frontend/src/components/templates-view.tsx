"use client";

import { useEffect, useMemo, useState } from "react";
import { ConfirmAction } from "@/components/confirm-action";
import { Check, Docker, GithubMark } from "@/components/icons";
import type { Source } from "@/lib/data";
import { fetchRepos, readCachedRepos } from "@/lib/repo-cache";
import { ADDONS, TEMPLATES, buildAIPrompt, findTemplate, suggestTemplate, templateFiles, type AddonId } from "@/lib/templates";

type Tree = { repo: string; branch: string; paths: string[] } | null;

const btn =
  "flex items-center gap-1.5 rounded-md border border-line bg-surface px-3 py-1.5 text-[12px] font-medium text-body transition-colors hover:border-line-strong hover:text-cream disabled:opacity-50";

// TemplatesView: pick a template, edit its files in place, then copy them, hand
// them to an AI with a prompt that spells out Berth's conventions, or commit
// them into a repo. Picking a repo reads its file list to suggest a template,
// flag files that would be overwritten, and give the AI real context.
export function TemplatesView({
  account,
  initialRepo,
  initialBranch,
  initialTemplate,
}: {
  account: string;
  initialRepo?: string;
  initialBranch?: string;
  initialTemplate?: string;
}) {
  const [templateId, setTemplateId] = useState(findTemplate(initialTemplate)?.id ?? TEMPLATES[0].id);
  // Hand edits, per template, by file path. Everything else is generated from
  // the template and the chosen add-ons.
  const [edits, setEdits] = useState<Record<string, Record<string, string>>>({});
  const [addonIds, setAddonIds] = useState<AddonId[]>([]);
  const [activePath, setActivePath] = useState<string>("Dockerfile");
  const [copied, setCopied] = useState<string | null>(null);

  const [repos, setRepos] = useState<Source[]>([]);
  const [repo, setRepo] = useState(initialRepo ?? "");
  const [branch, setBranch] = useState(initialBranch ?? "");
  const [tree, setTree] = useState<Tree>(null);
  const [treeError, setTreeError] = useState<string | null>(null);

  const [confirmCommit, setConfirmCommit] = useState(false);
  const [commitState, setCommitState] = useState<{ busy: boolean; result?: { ok: boolean; text: string } }>({ busy: false });

  const template = findTemplate(templateId) ?? TEMPLATES[0];
  const templateEdits = edits[template.id] ?? {};
  const files = templateFiles(template, template.noAddons ? [] : addonIds).map((f) =>
    f.path in templateEdits ? { ...f, content: templateEdits[f.path] } : f,
  );
  const active = files.find((f) => f.path === activePath) ?? files[0];
  const edited = Object.keys(templateEdits).length > 0;
  // Add-ons rewrite the compose file, so they lock once it is edited by hand.
  const addonsLocked = "docker-compose.yml" in templateEdits;

  function toggleAddon(id: AddonId) {
    const group = ADDONS.find((a) => a.id === id)?.group;
    setAddonIds((cur) => {
      if (cur.includes(id)) return cur.filter((x) => x !== id);
      // Add-ons in a group are alternatives: one database per app.
      const others = group ? cur.filter((x) => ADDONS.find((a) => a.id === x)?.group !== group) : cur;
      return [...others, id];
    });
  }

  // Repos for the picker: the browser cache first, the API on a miss.
  useEffect(() => {
    let live = true;
    const cached = readCachedRepos(account);
    (cached ? Promise.resolve(cached.repos) : fetchRepos())
      .then((list) => {
        if (live) setRepos(list);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [account]);

  const repoInfo = repos.find((r) => `${r.owner}/${r.name}` === repo);
  const effectiveBranch = branch || repoInfo?.branch || "";

  // The repo's file list: suggests a template and shows what would be overwritten.
  useEffect(() => {
    if (!repo.includes("/") || !effectiveBranch) return;
    const ctrl = new AbortController();
    const [owner, name] = repo.split("/");
    fetch(
      `/api/github/tree?owner=${encodeURIComponent(owner)}&repo=${encodeURIComponent(name)}&ref=${encodeURIComponent(effectiveBranch)}`,
      { cache: "no-store", signal: ctrl.signal },
    )
      .then(async (r) => {
        if (!r.ok) throw new Error(((await r.json().catch(() => ({}))) as { error?: string }).error || "couldn't read the repository");
        return (await r.json()) as { path: string; dir: boolean }[];
      })
      .then((entries) => {
        if (ctrl.signal.aborted) return;
        const paths = entries.filter((e) => !e.dir).map((e) => e.path);
        setTree({ repo, branch: effectiveBranch, paths });
        setTreeError(null);
        const s = suggestTemplate(paths);
        if (s && !initialTemplate) setTemplateId(s.id);
      })
      .catch((e: Error) => {
        if (ctrl.signal.aborted) return;
        setTree(null);
        setTreeError(e.message);
      });
    return () => ctrl.abort();
  }, [repo, effectiveBranch, initialTemplate]);

  const currentTree = tree && tree.repo === repo && tree.branch === effectiveBranch ? tree : null;
  const existing = useMemo(() => new Set(currentTree?.paths ?? []), [currentTree]);
  const suggested = currentTree ? suggestTemplate(currentTree.paths) : undefined;

  function updateFile(content: string) {
    setEdits((all) => ({ ...all, [template.id]: { ...(all[template.id] ?? {}), [active.path]: content } }));
  }

  async function copy(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(what);
      setTimeout(() => setCopied((c) => (c === what ? null : c)), 1800);
    } catch {
      setCopied(null);
    }
  }

  const prompt = buildAIPrompt(
    template,
    files,
    currentTree ? { name: repo, branch: currentTree.branch, paths: currentTree.paths } : undefined,
  );

  async function commit() {
    const [owner, name] = repo.split("/");
    setCommitState({ busy: true });
    try {
      const resp = await fetch(`/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/files`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ branch: effectiveBranch, message: `berth: add Docker setup (${template.name})`, files }),
      });
      const data = (await resp.json().catch(() => ({}))) as { error?: string };
      setCommitState({
        busy: false,
        result: resp.ok
          ? { ok: true, text: `Committed ${files.length} files to ${repo} on ${effectiveBranch}.` }
          : { ok: false, text: data.error || "The commit failed." },
      });
      if (resp.ok && currentTree) {
        setTree({ ...currentTree, paths: Array.from(new Set([...currentTree.paths, ...files.map((f) => f.path)])) });
      }
    } catch {
      setCommitState({ busy: false, result: { ok: false, text: "Couldn't reach Berth." } });
    }
  }

  const overwrites = files.filter((f) => existing.has(f.path)).map((f) => f.path);

  return (
    <div className="mt-6 grid gap-6 lg:grid-cols-[260px_minmax(0,1fr)]">
      {/* template list */}
      <nav className="flex gap-2 overflow-x-auto lg:flex-col lg:overflow-visible">
        {TEMPLATES.map((t) => {
          const on = t.id === template.id;
          return (
            <button
              key={t.id}
              onClick={() => {
                setTemplateId(t.id);
                setActivePath("Dockerfile");
              }}
              className={`shrink-0 rounded-lg border px-3 py-2.5 text-left transition-colors ${
                on ? "border-line-strong bg-surface-hover" : "border-line bg-surface hover:border-line-strong"
              }`}
            >
              <span className="flex items-center gap-2 text-[13px] font-medium text-cream">
                {t.name}
                {suggested?.id === t.id && (
                  <span className="rounded bg-blue/10 px-1.5 py-0.5 text-[10px] font-medium text-blue">suggested</span>
                )}
                {edits[t.id] && <span className="rounded bg-warn/10 px-1.5 py-0.5 text-[10px] font-medium text-warn">edited</span>}
              </span>
              <span className="mt-0.5 block text-[12px] text-muted">{t.stack}</span>
            </button>
          );
        })}
      </nav>

      <section className="min-w-0 space-y-4">
        {/* repo context */}
        <div className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-4 sm:flex-row sm:items-end">
          <label className="flex-1">
            <span className="text-[12px] font-medium text-muted">Repository (optional)</span>
            <input
              list="template-repos"
              value={repo}
              onChange={(e) => {
                setRepo(e.target.value.trim());
                setBranch("");
                setCommitState({ busy: false });
              }}
              placeholder="owner/name"
              className="mt-1 w-full rounded-md border border-line bg-surface-2 px-3 py-2 text-[13px] text-cream placeholder:text-faint focus:border-line-strong focus:outline-none"
            />
            <datalist id="template-repos">
              {repos.map((r) => (
                <option key={`${r.owner}/${r.name}`} value={`${r.owner}/${r.name}`} />
              ))}
            </datalist>
          </label>
          <label className="sm:w-48">
            <span className="text-[12px] font-medium text-muted">Branch</span>
            <input
              value={branch}
              onChange={(e) => setBranch(e.target.value.trim())}
              placeholder={repoInfo?.branch || "main"}
              className="mt-1 w-full rounded-md border border-line bg-surface-2 px-3 py-2 text-[13px] text-cream placeholder:text-faint focus:border-line-strong focus:outline-none"
            />
          </label>
        </div>
        {repo && (
          <p className="-mt-2 text-[12px] text-muted">
            {treeError
              ? <span className="text-err">{treeError}</span>
              : currentTree
                ? <>Read {currentTree.paths.length} files from <span className="text-cream">{repo}</span>.{suggested ? ` Looks like ${suggested.name}.` : ""} The AI prompt includes the file list.</>
                : repo.includes("/") && effectiveBranch ? "Reading the repository…" : "Pick a repository to suggest a template and commit to it."}
          </p>
        )}

        {/* template */}
        <div className="rounded-lg border border-line bg-surface">
          <div className="border-b border-line px-4 py-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="flex items-center gap-2 text-[15px] font-semibold text-cream">
                <Docker className="text-[#2496ED]" /> {template.name}
                <span className="text-[12px] font-normal text-muted">port {template.port}</span>
              </h2>
              <div className="flex flex-wrap items-center gap-2">
                {edited && (
                  <button
                    className={btn}
                    onClick={() =>
                      setEdits((all) => {
                        const next = { ...all };
                        delete next[template.id];
                        return next;
                      })
                    }
                  >
                    Reset
                  </button>
                )}
                <button className={btn} onClick={() => copy(active.content, "file")}>
                  {copied === "file" ? <Check /> : null} {copied === "file" ? "Copied" : `Copy ${active.path}`}
                </button>
                <button className={btn} onClick={() => copy(prompt, "prompt")}>
                  {copied === "prompt" ? <Check /> : null} {copied === "prompt" ? "Prompt copied" : "Copy AI prompt"}
                </button>
                <button
                  className="flex items-center gap-1.5 rounded-md bg-cream px-3 py-1.5 text-[12px] font-semibold text-bg transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:bg-surface-2 disabled:text-muted"
                  disabled={!repo.includes("/") || !effectiveBranch || commitState.busy}
                  title={repo.includes("/") ? undefined : "Pick a repository first"}
                  onClick={() => setConfirmCommit(true)}
                >
                  <GithubMark /> {commitState.busy ? "Committing…" : "Commit to repo"}
                </button>
              </div>
            </div>
            <p className="mt-1 text-[13px] text-muted">{template.description}</p>
            {template.notes.length > 0 && (
              <ul className="mt-2 list-disc space-y-0.5 pl-5 text-[12px] text-muted">
                {template.notes.map((n) => (
                  <li key={n}>{n}</li>
                ))}
              </ul>
            )}
            {!template.noAddons && (
              <div className="mt-3">
                <p className="text-[12px] font-medium text-cream">Add-ons</p>
                <div className="mt-1.5 flex flex-wrap gap-2">
                  {ADDONS.map((a) => {
                    const on = addonIds.includes(a.id);
                    return (
                      <button
                        key={a.id}
                        type="button"
                        aria-pressed={on}
                        disabled={addonsLocked}
                        title={a.description}
                        onClick={() => toggleAddon(a.id)}
                        className={`flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-[12px] font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                          on ? "border-line-strong bg-surface-hover text-cream" : "border-line text-muted hover:text-cream"
                        }`}
                      >
                        <span className={`h-1.5 w-1.5 rounded-full ${on ? "bg-ok" : "bg-faint"}`} />
                        {a.name}
                      </button>
                    );
                  })}
                </div>
                <p className="mt-1.5 text-[12px] text-muted">
                  {addonsLocked
                    ? "You edited docker-compose.yml, so add-ons are locked. Reset to change them."
                    : addonIds.length
                      ? "Each runs beside your app with a persistent volume and no public port. The app gets its connection URL and waits until it is healthy. Set the passwords listed in .env.example in Berth."
                      : "Run a database or cache next to your app, on the same server."}
                </p>
              </div>
            )}
          </div>

          {commitState.result && (
            <p
              role="status"
              className={`border-b border-line px-4 py-2 text-[13px] ${commitState.result.ok ? "text-ok" : "text-err"}`}
            >
              {commitState.result.text}
              {commitState.result.ok && " Next: New deployment → pick this repo."}
            </p>
          )}

          <div className="flex gap-4 overflow-x-auto border-b border-line px-4">
            {files.map((f) => (
              <button
                key={f.path}
                onClick={() => setActivePath(f.path)}
                className={`-mb-px flex shrink-0 items-center gap-1.5 border-b-2 py-2.5 text-[12px] font-medium transition-colors ${
                  f.path === active.path ? "border-cream text-cream" : "border-transparent text-muted hover:text-cream"
                }`}
              >
                <span className="data">{f.path}</span>
                {existing.has(f.path) && (
                  <span className="rounded bg-warn/10 px-1 text-[10px] text-warn" title="This file already exists in the repo">
                    exists
                  </span>
                )}
              </button>
            ))}
          </div>
          <textarea
            value={active.content}
            onChange={(e) => updateFile(e.target.value)}
            spellCheck={false}
            aria-label={`Edit ${active.path}`}
            className="data block h-[460px] w-full resize-y bg-surface-2 px-4 py-3 text-[12.5px] leading-relaxed text-cream focus:outline-none"
          />
        </div>
      </section>

      {confirmCommit && (
        <ConfirmAction
          title={`Commit ${template.name} setup to ${repo}`}
          subtitle={effectiveBranch}
          summary={
            <>
              Berth writes these files to <span className="text-cream">{repo}</span> on{" "}
              <span className="text-cream">{effectiveBranch}</span>{" "}using the GitHub App, one commit per file.
              {overwrites.length > 0 && (
                <>
                  {" "}
                  <span className="text-warn">
                    {overwrites.join(", ")} already exist{overwrites.length === 1 ? "s" : ""}{" "}and will be replaced.
                  </span>
                </>
              )}{" "}
              If the branch already deploys with Berth, these commits trigger a deploy.
            </>
          }
          stepsLabel="files"
          steps={files.map((f) => `${existing.has(f.path) ? "replace" : "add"} ${f.path}`)}
          confirmLabel="Commit files"
          destructive={overwrites.length > 0}
          onConfirm={() => {
            setConfirmCommit(false);
            void commit();
          }}
          onCancel={() => setConfirmCommit(false)}
        />
      )}
    </div>
  );
}

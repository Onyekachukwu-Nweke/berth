// Per-environment secrets, as the Go backend reports them. GitHub is
// authoritative for which names are configured (and their updated times);
// mountabo stores the revealable values it set, and flags drift.

// A secret changed on GitHub since mountabo synced it ("stale"), configured on
// GitHub with no local value ("setOutsideMountabo"), or a local value whose name
// is no longer on GitHub ("removedOnGitHub"). Empty when in sync.
export type SecretStatus = "stale" | "setOutsideMountabo" | "removedOnGitHub" | "";

// DeploymentSecret is one configured environment secret shaped for the UI.
export type DeploymentSecret = {
  name: string;
  // Managed connection secrets (SERVER_*, DEPLOY_DIR) are read-only.
  managed: boolean;
  // GitHub's last-updated time (RFC3339). Empty when only known locally.
  updatedAt: string;
  // True when mountabo holds a revealable value for it.
  hasLocalValue: boolean;
  // The value, present only when the list was fetched with includeValues.
  value?: string;
  status?: SecretStatus;
};

// DeploymentSecrets is the secrets-tab payload: the configured secrets plus the
// .env.example variable names not yet configured.
export type DeploymentSecrets = {
  secrets: DeploymentSecret[];
  missing: string[];
};

// SetSecretsResult reports a write. partial is true when the secret was set on
// GitHub but its value could not be stored locally (so it won't be revealable).
export type SetSecretsResult = {
  ok: boolean;
  partial: boolean;
  message?: string;
};

function deploymentPath(owner: string, repo: string, environment: string): string {
  return `/api/deployments/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${encodeURIComponent(environment)}`;
}

// fetchDeploymentSecrets lists an environment's configured secrets. Pass
// includeValues=true for the environment's own panel (values revealed behind a
// toggle); the cross-environment compare passes false so it never broadens
// plaintext exposure. Returns null when the backend can't be reached or the user
// isn't connected, so the panel shows an honest error rather than a blank list.
export async function fetchDeploymentSecrets(
  owner: string,
  repo: string,
  environment: string,
  includeValues: boolean,
  signal?: AbortSignal,
): Promise<DeploymentSecrets | null> {
  if (!owner || !repo || !environment) return null;
  try {
    const query = includeValues ? "?includeValues=true" : "";
    const resp = await fetch(`${deploymentPath(owner, repo, environment)}/secrets${query}`, {
      cache: "no-store",
      signal,
    });
    if (!resp.ok) return null;
    const data = (await resp.json()) as DeploymentSecrets;
    return {
      secrets: Array.isArray(data.secrets) ? data.secrets : [],
      missing: Array.isArray(data.missing) ? data.missing : [],
    };
  } catch {
    return null;
  }
}

// setDeploymentSecrets writes operator secrets to the environment (GitHub + the
// local encrypted store). Returns the result — including partial:true when the
// GitHub write succeeded but the local save didn't — or null on a transport
// failure. Managed names are rejected by the backend.
export async function setDeploymentSecrets(
  owner: string,
  repo: string,
  environment: string,
  secrets: { key: string; value: string }[],
): Promise<SetSecretsResult | null> {
  if (!owner || !repo || !environment || secrets.length === 0) return null;
  try {
    const resp = await fetch(`${deploymentPath(owner, repo, environment)}/secrets`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ secrets }),
    });
    if (!resp.ok) return null;
    return (await resp.json()) as SetSecretsResult;
  } catch {
    return null;
  }
}

// deleteDeploymentSecret removes one operator secret (GitHub + local value).
// Returns true only on a confirmed removal, so the panel refetches on success.
export async function deleteDeploymentSecret(
  owner: string,
  repo: string,
  environment: string,
  name: string,
): Promise<boolean> {
  if (!owner || !repo || !environment || !name) return false;
  try {
    const resp = await fetch(`${deploymentPath(owner, repo, environment)}/secrets/${encodeURIComponent(name)}`, {
      method: "DELETE",
    });
    return resp.ok;
  } catch {
    return false;
  }
}

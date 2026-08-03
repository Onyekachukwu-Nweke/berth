// Per-environment GitHub Actions secrets, as the Go backend reports them. GitHub
// is the single store: mountabo lists the configured names (+ updated times),
// sets values (write-only), and deletes secrets — but never reads a value back.

// DeploymentSecret is one configured environment secret. There is no value:
// GitHub Actions secret values are write-only and can't be retrieved.
export type DeploymentSecret = {
  name: string;
  // Managed connection secrets (SERVER_*, DEPLOY_DIR) are read-only.
  managed: boolean;
  // GitHub's last-updated time (RFC3339).
  updatedAt: string;
};

// DeploymentSecrets is the secrets-tab payload: the configured secrets plus the
// .env.example variable names not yet configured.
export type DeploymentSecrets = {
  secrets: DeploymentSecret[];
  missing: string[];
};

// ApplyResult reports the outcome of a secret change. ok is transport success
// (the write reached GitHub); redeployed is whether a redeploy was triggered to
// apply it to the running container; message explains a redeployed:false case
// (e.g. a legacy deployment that needs a full redeploy from configure).
export type ApplyResult = {
  ok: boolean;
  redeployed: boolean;
  message?: string;
};

function deploymentPath(owner: string, repo: string, environment: string): string {
  return `/api/deployments/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${encodeURIComponent(environment)}`;
}

// fetchDeploymentSecrets lists an environment's configured secret names (with
// GitHub's updated time and the managed flag) and the .env.example variables not
// yet configured. Returns null when the backend can't be reached or the user
// isn't connected, so the panel shows an honest error rather than a blank list.
export async function fetchDeploymentSecrets(
  owner: string,
  repo: string,
  environment: string,
  signal?: AbortSignal,
): Promise<DeploymentSecrets | null> {
  if (!owner || !repo || !environment) return null;
  try {
    const resp = await fetch(`${deploymentPath(owner, repo, environment)}/secrets`, {
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

// setDeploymentSecrets writes operator secrets to the GitHub environment
// (write-only) and re-applies the deployment. Returns the apply result: ok=false
// on any transport failure; on success, redeployed + message from the backend.
// Managed / invalid / multiline are rejected by the backend (ok=false).
export async function setDeploymentSecrets(
  owner: string,
  repo: string,
  environment: string,
  secrets: { key: string; value: string }[],
): Promise<ApplyResult> {
  if (!owner || !repo || !environment || secrets.length === 0) return { ok: false, redeployed: false };
  try {
    const resp = await fetch(`${deploymentPath(owner, repo, environment)}/secrets`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ secrets }),
    });
    if (!resp.ok) return { ok: false, redeployed: false };
    const data = (await resp.json()) as { redeployed?: boolean; message?: string };
    return { ok: true, redeployed: Boolean(data.redeployed), message: data.message };
  } catch {
    return { ok: false, redeployed: false };
  }
}

// deleteDeploymentSecret removes one operator secret from the GitHub environment
// and re-applies the deployment. Returns the apply result (see setDeploymentSecrets).
export async function deleteDeploymentSecret(
  owner: string,
  repo: string,
  environment: string,
  name: string,
): Promise<ApplyResult> {
  if (!owner || !repo || !environment || !name) return { ok: false, redeployed: false };
  try {
    const resp = await fetch(`${deploymentPath(owner, repo, environment)}/secrets/${encodeURIComponent(name)}`, {
      method: "DELETE",
    });
    if (!resp.ok) return { ok: false, redeployed: false };
    const data = (await resp.json()) as { redeployed?: boolean; message?: string };
    return { ok: true, redeployed: Boolean(data.redeployed), message: data.message };
  } catch {
    return { ok: false, redeployed: false };
  }
}

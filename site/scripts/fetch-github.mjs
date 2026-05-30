#!/usr/bin/env node
// Fetch GitHub repo metrics into src/content/github/snapshot.json.
//
// Used by the engineering dashboard at /engineering/.
//
// Auth strategy:
//   1. If GITHUB_TOKEN is set (Cloudflare Pages env, CI, local export):
//      use plain fetch() against api.github.com. Required for private repos.
//   2. Else, fall back to `gh` / `gh.exe` (whichever is on PATH).
//   3. Else, fail soft: write a stub snapshot so the site still builds.
//
// The build environment on Cloudflare Pages has no `gh` binary — set
// GITHUB_TOKEN there. Locally on WSL, `gh.exe` is the convention.

import { writeFileSync, mkdirSync, existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = resolve(HERE, "..", "src", "content", "github", "snapshot.json");

const REPO = process.env.GITHUB_REPO ?? "mscrnt/artist-alley";

const HEADERS_BASE = {
  Accept: "application/vnd.github+json",
  "X-GitHub-Api-Version": "2022-11-28",
  "User-Agent": "artist-alley-site-build",
};

// --- transport: fetch() or gh CLI ------------------------------------------
//
// Priority order:
//   1. gh / gh.exe on PATH (local dev) — uses the user's logged-in auth which
//      is usually scoped correctly for private repos.
//   2. GITHUB_TOKEN / GH_TOKEN env var (CI, Cloudflare Pages) — only used
//      when gh isn't available, so a stale local PAT in the env doesn't
//      poison the local dev experience.
//   3. Stub snapshot — keeps the build green when neither is available.

const ghBinary = pickGhBinary();
// Only honor the env token when gh isn't available; otherwise stale PATs
// silently overrule a working `gh auth login`.
const token = ghBinary
  ? null
  : (process.env.GITHUB_TOKEN || process.env.GH_TOKEN || null);

function pickGhBinary() {
  // Try gh.exe first on WSL (the user's documented preference); fall back to gh.
  const candidates = process.env.WSL_DISTRO_NAME || process.platform === "win32"
    ? ["gh.exe", "gh"]
    : ["gh", "gh.exe"];
  for (const candidate of candidates) {
    try {
      execFileSync(candidate, ["--version"], { stdio: "ignore" });
      return candidate;
    } catch {
      // continue
    }
  }
  return null;
}

async function gh(pathAndQuery) {
  if (token) {
    const url = `https://api.github.com${pathAndQuery.startsWith("/") ? "" : "/"}${pathAndQuery}`;
    const res = await fetch(url, {
      headers: { ...HEADERS_BASE, Authorization: `Bearer ${token}` },
    });
    if (res.status === 202) {
      // Stats endpoints return 202 while precomputing — best-effort retry.
      await new Promise((r) => setTimeout(r, 1500));
      return gh(pathAndQuery);
    }
    if (!res.ok) {
      throw new Error(`GitHub API ${res.status} on ${pathAndQuery}: ${await res.text()}`);
    }
    return res.json();
  }
  if (ghBinary) {
    const out = execFileSync(ghBinary, ["api", pathAndQuery], { encoding: "utf8" });
    return JSON.parse(out);
  }
  throw new Error("No auth: set GITHUB_TOKEN or install gh / gh.exe");
}

// --- main ------------------------------------------------------------------

async function main() {
  console.log(`[fetch-github] repo=${REPO} auth=${token ? "token" : ghBinary ?? "none"}`);

  if (!token && !ghBinary) {
    console.warn("[fetch-github] no auth available — writing stub snapshot");
    writeStub("no-auth");
    return;
  }

  try {
    const [repo, languages, contributors, openIssuesRaw, closedIssuesRaw, openPullsRaw, closedPullsRaw, commitsRaw, releases, commitActivity] =
      await Promise.all([
        gh(`/repos/${REPO}`),
        gh(`/repos/${REPO}/languages`),
        gh(`/repos/${REPO}/contributors?per_page=30`),
        gh(`/repos/${REPO}/issues?state=open&per_page=50`),
        gh(`/repos/${REPO}/issues?state=closed&per_page=30&sort=updated&direction=desc`),
        gh(`/repos/${REPO}/pulls?state=open&per_page=30`),
        gh(`/repos/${REPO}/pulls?state=closed&per_page=20&sort=updated&direction=desc`),
        gh(`/repos/${REPO}/commits?per_page=30`),
        gh(`/repos/${REPO}/releases?per_page=20`),
        ghStats(`/repos/${REPO}/stats/commit_activity`),
      ]);

    // The /issues endpoint returns PRs intermixed; filter them out for issues.
    const realIssues = (raw) =>
      raw.filter((i) => !i.pull_request).map(normalizeIssueOrPR);

    const snapshot = {
      fetchedAt: new Date().toISOString(),
      repo: {
        full_name: repo.full_name,
        description: repo.description,
        html_url: repo.html_url,
        homepage: repo.homepage,
        default_branch: repo.default_branch,
        visibility: repo.visibility,
        stargazers_count: repo.stargazers_count,
        watchers_count: repo.watchers_count,
        forks_count: repo.forks_count,
        open_issues_count: repo.open_issues_count,
        subscribers_count: repo.subscribers_count,
        pushed_at: repo.pushed_at,
        created_at: repo.created_at,
        license: repo.license ? { spdx_id: repo.license.spdx_id } : null,
        topics: repo.topics ?? [],
      },
      languages,
      contributors: (Array.isArray(contributors) ? contributors : []).map((c) => ({
        login: c.login,
        avatar_url: c.avatar_url,
        html_url: c.html_url,
        contributions: c.contributions,
      })),
      issues: {
        open: realIssues(openIssuesRaw),
        recentlyClosed: realIssues(closedIssuesRaw),
      },
      pulls: {
        open: openPullsRaw.map(normalizeIssueOrPR),
        recentlyMerged: closedPullsRaw.filter((p) => p.merged_at).map(normalizeIssueOrPR),
      },
      commits: commitsRaw.map((c) => ({
        sha: c.sha,
        message: (c.commit?.message ?? "").split("\n")[0].slice(0, 200),
        author: c.commit?.author?.name ?? c.author?.login ?? null,
        date: c.commit?.author?.date ?? c.commit?.committer?.date ?? "",
        url: c.html_url,
      })),
      releases: releases.map((r) => ({
        tag_name: r.tag_name,
        name: r.name,
        html_url: r.html_url,
        published_at: r.published_at,
        prerelease: r.prerelease,
        draft: r.draft,
      })),
      commitActivity: Array.isArray(commitActivity) ? commitActivity : [],
    };

    mkdirSync(dirname(OUT), { recursive: true });
    writeFileSync(OUT, JSON.stringify(snapshot, null, 2) + "\n", "utf8");
    console.log(
      `[fetch-github] wrote ${relativeOut()} (issues: ${snapshot.issues.open.length} open, ` +
        `${snapshot.issues.recentlyClosed.length} recently closed; ` +
        `pulls: ${snapshot.pulls.open.length} open, ` +
        `${snapshot.pulls.recentlyMerged.length} merged; ` +
        `commits: ${snapshot.commits.length}; releases: ${snapshot.releases.length})`
    );
  } catch (err) {
    console.error(`[fetch-github] failed: ${err.message}`);
    if (existsSync(OUT)) {
      console.warn("[fetch-github] keeping previous snapshot");
      return;
    }
    writeStub(`error: ${err.message}`);
  }
}

async function ghStats(path) {
  // Stats endpoints may 202 while GitHub precomputes; gh() handles one retry.
  try {
    return await gh(path);
  } catch (err) {
    console.warn(`[fetch-github] stats endpoint failed (${path}): ${err.message}`);
    return [];
  }
}

function normalizeIssueOrPR(x) {
  return {
    number: x.number,
    title: x.title,
    state: x.state,
    html_url: x.html_url,
    user: x.user
      ? { login: x.user.login, avatar_url: x.user.avatar_url, html_url: x.user.html_url }
      : null,
    labels: (x.labels ?? []).map((l) =>
      typeof l === "string"
        ? { name: l }
        : { name: l.name, color: l.color, description: l.description }
    ),
    created_at: x.created_at,
    updated_at: x.updated_at,
    closed_at: x.closed_at,
    merged_at: x.merged_at,
    draft: x.draft,
    comments: x.comments,
  };
}

function writeStub(reason) {
  const stub = {
    fetchedAt: new Date().toISOString(),
    repo: {
      full_name: REPO,
      description: null,
      html_url: `https://github.com/${REPO}`,
      homepage: null,
      default_branch: "main",
      visibility: "unknown",
      stargazers_count: 0,
      watchers_count: 0,
      forks_count: 0,
      open_issues_count: 0,
      topics: [],
    },
    languages: {},
    contributors: [],
    issues: { open: [], recentlyClosed: [] },
    pulls: { open: [], recentlyMerged: [] },
    commits: [],
    releases: [],
    commitActivity: [],
    _stub: reason,
  };
  mkdirSync(dirname(OUT), { recursive: true });
  writeFileSync(OUT, JSON.stringify(stub, null, 2) + "\n", "utf8");
  console.log(`[fetch-github] wrote stub snapshot (${reason})`);
}

function relativeOut() {
  return OUT.replace(resolve(HERE, "..") + "/", "");
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});

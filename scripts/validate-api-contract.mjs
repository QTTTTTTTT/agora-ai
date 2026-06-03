#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const root = path.resolve(process.argv[2] || ".");
const serverRoots = ["server/cmd/server", "server/internal/api"];
const clientRoots = ["web/src", "miniapp"];
const errors = [];
const allowedDynamicClientPatterns = new Set([
  "ANY /api/abtests/{param}/{param}", // web uses a validated action variable: start | stop | analyze
]);

function walk(dir, predicate = () => true, acc = []) {
  if (!fs.existsSync(dir)) return acc;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name === "node_modules" || entry.name === "dist") continue;
      walk(file, predicate, acc);
    } else if (predicate(file)) {
      acc.push(file);
    }
  }
  return acc;
}

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, "/");
}

function normalizePath(raw) {
  return raw
    .replace(/\$\{[^}]+\}/g, "{param}")
    .replace(/\$\{[^/]*$/g, "")
    .replace(/([^/])\{param\}.*$/g, "$1")
    .replace(/\?.*$/, "")
    .replace(/\/+/g, "/")
    .replace(/\/$/, "") || "/";
}

function routeToRegex(routePath) {
  const escaped = routePath
    .replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
    .replace(/\\\{[^/]+\\\}/g, "[^/]+");
  return new RegExp(`^${escaped}$`);
}

function extractBackendRoutes() {
  const routes = [];
  for (const dir of serverRoots.map((item) => path.join(root, item))) {
    for (const file of walk(dir, (candidate) => candidate.endsWith(".go"))) {
      const text = fs.readFileSync(file, "utf8");
      const pattern = /HandleFunc\("([A-Z]+)\s+(\/api[^"\s]+)"/g;
      for (const match of text.matchAll(pattern)) {
        routes.push({ method: match[1], path: normalizePath(match[2]), file: rel(file) });
      }
    }
  }
  return routes;
}

function extractStringLiterals(line) {
  const literals = [];
  const pattern = /(['"`])((?:\\.|(?!\1).)*\/api(?:\\.|(?!\1).)*)\1/g;
  for (const match of line.matchAll(pattern)) literals.push(match[2]);
  return literals;
}

function extractClientCalls() {
  const calls = [];
  for (const dir of clientRoots.map((item) => path.join(root, item))) {
    for (const file of walk(dir, (candidate) => /\.(?:ts|tsx|js)$/.test(candidate))) {
      const lines = fs.readFileSync(file, "utf8").split(/\r?\n/);
      for (const [index, line] of lines.entries()) {
        if (!line.includes("/api/")) continue;
        // Method inference is order-sensitive to avoid picking up
        // a neighbouring helper call: we probe the literal-bearing
        // line first (`apiGet(\`/api/...\`)`), then the 1-2 lines
        // immediately above (`apiPost<T>(\n  url, body)`), then
        // the 4 lines below — but only when the line itself opens
        // a fetch/request that carries options on subsequent lines.
        const backward = lines.slice(Math.max(0, index - 2), index).join("\n");
        const forward = lines.slice(index + 1, index + 5).join("\n");
        const method = inferMethod({ line, backward, forward });
        const literals = extractStringLiterals(line);
        const hasConcatenation = line.includes("+");
        for (const literal of literals) {
          const raw = literal.replace(/\\'/g, "'").replace(/\\"/g, '"');
          const start = raw.indexOf("/api/");
          if (start < 0) continue;
          const pathValue = normalizePath(raw.slice(start));
          if (hasConcatenation && /\/api\/[^?]*\/$/.test(raw.slice(start))) continue;
          calls.push({ method, path: pathValue, file: rel(file), line: index + 1 });
        }

        // Handle common one-line concatenations such as '/api/funds/' + fundId + '/team'.
        const quotedPieces = [...line.matchAll(/(['"`])((?:\\.|(?!\1).)*)\1/g)].map((match) => match[2]);
        const apiPieceIndex = quotedPieces.findIndex((piece) => piece.includes("/api/"));
        if (hasConcatenation && apiPieceIndex >= 0) {
          const relevant = quotedPieces.slice(apiPieceIndex).filter((piece) => !piece.startsWith("http"));
          if (relevant.length > 1) {
            const joined = relevant
              .map((piece, idx) => (idx === 0 ? piece : `{param}${piece}`))
              .join("");
            // A trailing `+ identifier` extends the path with one
            // more {param}. We recognise both terminal forms
            // ('...+x);' and '...+x, body);') so miniapp helpers
            // that pass an extra payload after a path param still
            // produce the full route shape.
            const trimmed = line.trim();
            const trailingParam = /\+\s*[A-Za-z_$][\w$.[\]]*\s*(?:,[^)]*)?\)?\s*;?\s*$/.test(trimmed);
            const suffix = trailingParam ? "{param}" : "";
            calls.push({ method, path: normalizePath(joined.slice(joined.indexOf("/api/")) + suffix), file: rel(file), line: index + 1 });
          }
        }
      }
    }
  }
  return dedupeCalls(calls);
}

// inferMethod heuristically tags an /api/ literal with a method.
// It probes three slices in priority order to avoid leaking
// method hints from neighbouring helper calls:
//   1. the literal-bearing line (covers `apiGet(\`/api/...\`)`),
//   2. the 1-2 lines immediately above (covers
//      `apiPost<T>(\n  \`/api/...\`, body)` where the call site
//      sits on the previous line),
//   3. the 1-4 lines immediately below, but only when the line
//      itself opens a fetch/request that carries options on
//      subsequent lines (covers `fetch('/api/...', { method:`).
function inferMethod({ line, backward, forward }) {
  const probe = (text) => {
    const lower = text.toLowerCase();
    if (lower.includes("submitauth")) return "POST";
    if (lower.includes("apipost") || /\bpost\s*\(/.test(text) || /method\s*:\s*['"]post['"]/i.test(text)) return "POST";
    if (lower.includes("apiput") || /\bput\s*\(/.test(text) || /method\s*:\s*['"]put['"]/i.test(text)) return "PUT";
    if (lower.includes("apipatch") || /\bpatch\s*\(/.test(text) || /method\s*:\s*['"]patch['"]/i.test(text)) return "PATCH";
    if (lower.includes("apidelete") || /\bdel\s*\(/.test(text) || /method\s*:\s*['"]delete['"]/i.test(text)) return "DELETE";
    if (/request\s*\([^,)]*,\s*['"`]POST['"`]/i.test(text)) return "POST";
    if (/request\s*\([^,)]*,\s*['"`]PUT['"`]/i.test(text)) return "PUT";
    if (/request\s*\([^,)]*,\s*['"`]PATCH['"`]/i.test(text)) return "PATCH";
    if (/request\s*\([^,)]*,\s*['"`]DELETE['"`]/i.test(text)) return "DELETE";
    // Explicit GET hint stops the inference here so a literal
    // sitting next to a sibling POST helper isn't mis-tagged.
    // `\bget\s*\(` matches `get(` but NOT `getSomething(`.
    if (lower.includes("apiget") || /\bget\s*\(/.test(text) || /method\s*:\s*['"]get['"]/i.test(text)) return "GET";
    return null;
  };
  const fromLine = probe(line);
  if (fromLine) return fromLine;
  const fromBackward = probe(backward);
  if (fromBackward) return fromBackward;
  // Forward look only when the line plausibly opens a multi-line
  // fetch / jsonRequest / apiRequest / request invocation whose
  // options object carries the verb. The `(?:<[^>]+>)?` slot lets
  // us match generic helpers like `jsonRequest<Resp>(url, {`
  // without overreaching to unrelated calls.
  if (/\b(?:fetch|request|jsonRequest|apiRequest)(?:<[^>]+>)?\s*\(|\burl\s*:/.test(line)) {
    const fromForward = probe(forward);
    if (fromForward) return fromForward;
  }
  return "GET";
}

function dedupeCalls(calls) {
  const seen = new Set();
  return calls.filter((call) => {
    const key = `${call.method} ${call.path} ${call.file}:${call.line}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

function matchesRoute(call, routes) {
  return routes.some((route) => {
    if (call.method !== route.method && call.method !== "ANY") return false;
    return routeToRegex(route.path).test(call.path);
  });
}

const routes = extractBackendRoutes();
const calls = extractClientCalls();

if (routes.length === 0) errors.push("no backend API routes found");

for (const call of calls) {
  const key = `${call.method} ${call.path}`;
  if (allowedDynamicClientPatterns.has(key) || allowedDynamicClientPatterns.has(`ANY ${call.path}`)) continue;
  if (!matchesRoute(call, routes)) {
    errors.push(`${call.file}:${call.line}: no backend route for ${call.method} ${call.path}`);
  }
}

if (errors.length > 0) {
  console.error("API contract validation failed:");
  for (const error of errors) console.error(`- ${error}`);
  process.exit(1);
}

console.log(`backend_routes=${routes.length}`);
console.log(`client_api_calls=${calls.length}`);
console.log("api_contract_validation=ok");

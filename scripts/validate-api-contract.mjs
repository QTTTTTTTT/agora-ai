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
        const context = lines.slice(index, index + 5).join("\n");
        const method = inferMethod(/\burl\s*:|fetch\s*\(/.test(line) ? context : line);
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
            const suffix = /\+\s*[A-Za-z_$][\w$.[\]]*\s*\)?\s*;?$/.test(line.trim()) ? "{param}" : "";
            calls.push({ method, path: normalizePath(joined.slice(joined.indexOf("/api/")) + suffix), file: rel(file), line: index + 1 });
          }
        }
      }
    }
  }
  return dedupeCalls(calls);
}

function inferMethod(line) {
  const lower = line.toLowerCase();
  if (lower.includes("submitauth")) return "POST";
  if (lower.includes("apipost") || lower.includes("post(") || /method:\s*['"]post['"]/i.test(line)) return "POST";
  if (lower.includes("apiput") || lower.includes("put(") || /method:\s*['"]put['"]/i.test(line)) return "PUT";
  if (lower.includes("apidelete") || lower.includes("del(") || /method:\s*['"]delete['"]/i.test(line)) return "DELETE";
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

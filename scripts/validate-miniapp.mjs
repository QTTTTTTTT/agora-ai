#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const root = path.resolve(process.argv[2] || "miniapp");
const requiredAppFiles = ["app.js", "app.json", "app.wxss", "project.config.json", "sitemap.json"];
const pageExts = [".js", ".json", ".wxml", ".wxss"];
const errors = [];

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, "/");
}

function requireFile(file, label = file) {
  if (!fs.existsSync(file) || !fs.statSync(file).isFile()) {
    errors.push(`missing ${label}: ${rel(file)}`);
  }
}

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (error) {
    errors.push(`invalid json ${rel(file)}: ${error.message}`);
    return undefined;
  }
}

function walk(dir, predicate = () => true, acc = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(file, predicate, acc);
    } else if (predicate(file)) {
      acc.push(file);
    }
  }
  return acc;
}

function validatePage(pagePath, owner) {
  const base = path.join(root, pagePath);
  for (const ext of pageExts) {
    requireFile(`${base}${ext}`, `${owner} page artifact`);
  }
}

if (!fs.existsSync(root) || !fs.statSync(root).isDirectory()) {
  throw new Error(`miniapp root does not exist: ${root}`);
}

for (const file of requiredAppFiles) {
  requireFile(path.join(root, file), "required miniapp file");
}

for (const file of walk(root, (candidate) => candidate.endsWith(".json"))) {
  readJSON(file);
}

const app = readJSON(path.join(root, "app.json"));
if (app) {
  if (!Array.isArray(app.pages) || app.pages.length === 0) {
    errors.push("app.json pages must be a non-empty array");
  } else {
    for (const page of app.pages) validatePage(page, "app.json");
  }

  if (Array.isArray(app.subPackages)) {
    for (const pkg of app.subPackages) {
      if (!pkg.root || !Array.isArray(pkg.pages)) {
        errors.push(`invalid subPackage entry: ${JSON.stringify(pkg)}`);
        continue;
      }
      for (const page of pkg.pages) validatePage(path.posix.join(pkg.root, page), `subPackage ${pkg.root}`);
    }
  }

  for (const tab of app.tabBar?.list || []) {
    if (tab.pagePath) validatePage(tab.pagePath, "tabBar");
    if (tab.iconPath) requireFile(path.join(root, tab.iconPath), "tabBar icon");
    if (tab.selectedIconPath) requireFile(path.join(root, tab.selectedIconPath), "tabBar selected icon");
  }
}

const jsonFiles = walk(root, (candidate) => candidate.endsWith(".json"));
for (const file of jsonFiles) {
  const config = readJSON(file);
  if (!config?.usingComponents) continue;
  for (const [name, componentPath] of Object.entries(config.usingComponents)) {
    if (typeof componentPath !== "string" || componentPath.trim() === "") {
      errors.push(`invalid usingComponents.${name} in ${rel(file)}`);
      continue;
    }
    const normalized = componentPath.startsWith("/") ? componentPath.slice(1) : path.posix.join(path.posix.dirname(rel(file)), componentPath);
    const base = path.join(root, normalized);
    for (const ext of pageExts) requireFile(`${base}${ext}`, `component ${name}`);
  }
}

for (const file of walk(root, (candidate) => candidate.endsWith(".js"))) {
  const result = spawnSync(process.execPath, ["--check", file], { encoding: "utf8" });
  if (result.status !== 0) {
    errors.push(`javascript syntax error ${rel(file)}:\n${result.stderr || result.stdout}`);
  }
}

if (errors.length > 0) {
  console.error("Miniapp validation failed:");
  for (const error of errors) console.error(`- ${error}`);
  process.exit(1);
}

console.log(`miniapp_root=${root}`);
console.log(`json_files=${jsonFiles.length}`);
console.log(`js_files=${walk(root, (candidate) => candidate.endsWith(".js")).length}`);
console.log("miniapp_validation=ok");

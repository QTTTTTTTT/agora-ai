// Package cn_tactics embeds the 4 A-share short-term tactic persona
// JSON templates (尾盘狙击/首板低吸/龙头打板/缩量回踩) and exposes them
// as in-memory bytes.
//
// Mirrors the pattern in internal/agent/masters/embed.go — see that
// file for the design rationale (single embed.FS, no behaviour).
package cn_tactics

import "embed"

// FS is the embedded filesystem holding every <key>.json under
// this directory. Filenames double as the tactic key.
//
//go:embed *.json
var FS embed.FS

// MascotAvatar — pixel-art role avatars for the agent team.
//
// The reference design uses chunky retro-RPG style portraits
// (策略队长 / 情报 / 选股 / 交易 / 风控). We can't ship raster
// assets cleanly without bloating the bundle, so each role is
// drawn as a parameterised inline SVG: a 16×16 pixel grid is
// rendered through `<rect>` cells, with a per-role palette and
// a small accessory layer on top (helmet trim, headset, shield…).
//
// This is intentionally low-fidelity vs hand-drawn artwork —
// the goal is the *silhouette + color language*, which is what
// the user recognises at thumbnail size. Designers can replace
// the SVG with hand-crafted PNGs later by swapping the
// `roleArt` map; the public API stays identical.

import React from "react";

export type MascotRole =
  | "captain"   // 策略队长 — uniform + cap, gold trim
  | "intel"     // 情报      — headset + newspaper
  | "picker"   // 选股      — headphones + magnifier
  | "trader"   // 交易      — BUY/SELL board
  | "risk"     // 风控      — red cap + shield
  | "analyst"; // 分析师    — glasses + ledger

interface MascotPalette {
  bg:      string; // soft circle background
  hair:    string;
  skin:    string;
  uniform: string;
  accent:  string; // hat trim / shield / badge
  mouth:   string;
}

const palettes: Record<MascotRole, MascotPalette> = {
  captain: { bg: "#f0ebe0", hair: "#1f1d18", skin: "#f3d2b1", uniform: "#1f3a52", accent: "#d4a75c", mouth: "#7a4a2e" },
  intel:   { bg: "#e6f0e8", hair: "#5b3b25", skin: "#f3d2b1", uniform: "#7da1a8", accent: "#3a6f78", mouth: "#7a4a2e" },
  picker:  { bg: "#f1ece2", hair: "#3b2418", skin: "#f3d2b1", uniform: "#7d6dc4", accent: "#3a3372", mouth: "#7a4a2e" },
  trader:  { bg: "#e9f1e2", hair: "#1f1d18", skin: "#f3d2b1", uniform: "#3a3a3a", accent: "#1faa64", mouth: "#7a4a2e" },
  risk:    { bg: "#f4e6e0", hair: "#1f1d18", skin: "#f3d2b1", uniform: "#3a3a3a", accent: "#c5343a", mouth: "#7a4a2e" },
  analyst: { bg: "#eef2ff", hair: "#3b2418", skin: "#f3d2b1", uniform: "#4a4a52", accent: "#6b6f7a", mouth: "#7a4a2e" },
};

// Each character is described by a 16×16 grid of palette letters.
// Letters map to palette slots:
//   . = transparent   B = background circle (handled separately)
//   H = hair          S = skin             U = uniform
//   A = accent        M = mouth            E = eye (#1a1a1a)
//   W = white         K = black (border)
const grid = (rows: string[]): string[][] =>
  rows.map((r) => r.split(""));

const baseFigure = grid([
  "................",
  "................",
  "....HHHHHHHH....",
  "...HHHHHHHHHH...",
  "..HSSSSSSSSSSH..",
  "..HSEESSSEESSH..",
  "..HSSSSSSSSSSH..",
  "..HSSSMMMMSSSH..",
  "...HSSSSSSSSH...",
  "...UUUUUUUUUU...",
  "..UUAAAAAAAAUU..",
  ".UUAAAAAAAAAAUU.",
  ".UAAAAAAAAAAAAU.",
  ".UAAAAAAAAAAAAU.",
  ".UUAAAAAAAAAAUU.",
  "..UUUUUUUUUUUU..",
]);

// Per-role accessories overlay the base figure. They live on
// top of the figure (drawn last) so e.g. a cap covers the hair.
type Accessory =
  | { type: "cap"; color: string }                // covers top of head
  | { type: "headset"; color: string }             // ear cups + mic
  | { type: "shield"; color: string }              // chest shield emblem
  | { type: "monitor"; color: string; label: string } // BUY/SELL board
  | { type: "newspaper"; color: string; label: string }
  | { type: "glasses"; color: string };

const accessories: Record<MascotRole, Accessory[]> = {
  captain: [
    { type: "cap",     color: "#0f1f30" },
  ],
  intel: [
    { type: "headset", color: "#3a6f78" },
    { type: "newspaper", color: "#fdebec", label: "NEWS" },
  ],
  picker: [
    { type: "headset", color: "#3a3372" },
    { type: "glasses", color: "#1f1d18" },
  ],
  trader: [
    { type: "monitor", color: "#1f1d18", label: "BUY/SELL" },
  ],
  risk: [
    { type: "cap",    color: "#c5343a" },
    { type: "shield", color: "#c5343a" },
  ],
  analyst: [
    { type: "glasses", color: "#1f1d18" },
  ],
};

// Render a single accessory on top of the figure as additional
// <rect> elements positioned in the same 16-cell grid system.
const renderAccessory = (a: Accessory, key: string, cell: number): React.ReactNode => {
  switch (a.type) {
    case "cap":
      return (
        <g key={key}>
          {/* cap brim */}
          <rect x={2 * cell} y={2 * cell} width={12 * cell} height={cell} fill={a.color} />
          {/* cap body */}
          <rect x={3 * cell} y={cell} width={10 * cell} height={cell} fill={a.color} />
          <rect x={4 * cell} y={0} width={8 * cell} height={cell} fill={a.color} />
          {/* gold center trim — captain-style */}
          <rect x={7 * cell} y={cell} width={2 * cell} height={cell} fill="#d4a75c" />
        </g>
      );
    case "headset":
      return (
        <g key={key}>
          {/* arc band on top of head */}
          <rect x={4 * cell} y={2 * cell} width={8 * cell} height={cell} fill={a.color} />
          <rect x={3 * cell} y={3 * cell} width={cell} height={2 * cell} fill={a.color} />
          <rect x={12 * cell} y={3 * cell} width={cell} height={2 * cell} fill={a.color} />
          {/* ear cups */}
          <rect x={2 * cell} y={5 * cell} width={2 * cell} height={2 * cell} fill={a.color} />
          <rect x={12 * cell} y={5 * cell} width={2 * cell} height={2 * cell} fill={a.color} />
        </g>
      );
    case "shield":
      return (
        <g key={key}>
          <rect x={6 * cell} y={11 * cell} width={4 * cell} height={cell} fill="#fff" />
          <rect x={5 * cell} y={12 * cell} width={6 * cell} height={2 * cell} fill={a.color} />
          <rect x={6 * cell} y={14 * cell} width={4 * cell} height={cell} fill={a.color} />
          <rect x={7 * cell} y={12 * cell} width={2 * cell} height={cell} fill="#fff" />
        </g>
      );
    case "monitor":
      return (
        <g key={key}>
          {/* small monitor on chest */}
          <rect x={5 * cell} y={11 * cell} width={6 * cell} height={3 * cell} fill={a.color} />
          <rect x={6 * cell} y={12 * cell} width={4 * cell} height={cell} fill="#1faa64" />
          <text x={8 * cell} y={12.7 * cell} textAnchor="middle" fontSize={cell * 0.9} fontFamily="ui-monospace, monospace" fill="#fff">
            {a.label}
          </text>
        </g>
      );
    case "newspaper":
      return (
        <g key={key}>
          <rect x={11 * cell} y={11 * cell} width={4 * cell} height={3 * cell} fill={a.color} />
          <rect x={12 * cell} y={12 * cell} width={2 * cell} height={cell} fill="#1f1d18" />
          <rect x={12 * cell} y={13 * cell} width={2 * cell} height={cell * 0.5} fill="#1f1d18" />
        </g>
      );
    case "glasses":
      return (
        <g key={key}>
          <rect x={4 * cell}  y={5 * cell} width={3 * cell} height={2 * cell} fill="none" stroke={a.color} strokeWidth={cell * 0.4} />
          <rect x={9 * cell}  y={5 * cell} width={3 * cell} height={2 * cell} fill="none" stroke={a.color} strokeWidth={cell * 0.4} />
          <rect x={7 * cell}  y={5.7 * cell} width={2 * cell} height={cell * 0.4} fill={a.color} />
        </g>
      );
  }
};

export interface MascotAvatarProps extends React.HTMLAttributes<HTMLDivElement> {
  role: MascotRole;
  /** Diameter in pixels of the rounded background. Default 88. */
  size?: number;
  /** Show a circle bg behind the character (default true). */
  withBackground?: boolean;
  /** Add a gentle bob animation — useful in 阵容 cards. */
  animated?: boolean;
}

export const MascotAvatar: React.FC<MascotAvatarProps> = ({
  role,
  size = 88,
  withBackground = true,
  animated = false,
  className = "",
  ...rest
}) => {
  const palette = palettes[role];
  const cell = 16; // 16 logical units per cell so the SVG is 256 wide
  const grid16 = baseFigure;

  // Map letters to palette colors, supplied at draw time so each
  // role gets its own uniform / hair / accent without re-typing
  // the grid for every character.
  const colorFor = (ch: string): string | null => {
    switch (ch) {
      case "H": return palette.hair;
      case "S": return palette.skin;
      case "U": return palette.uniform;
      case "A": return palette.accent;
      case "M": return palette.mouth;
      case "E": return "#1a1a1a";
      case "W": return "#ffffff";
      case "K": return "#1a1a1a";
      default:  return null;
    }
  };

  return (
    <div
      className={[
        "inline-flex shrink-0 items-center justify-center pixel-rendering",
        animated ? "animate-mascot-bob" : "",
        withBackground ? "rounded-2xl" : "",
        className,
      ].join(" ")}
      style={{
        width: size,
        height: size,
        background: withBackground ? palette.bg : undefined,
      }}
      aria-label={`${role} mascot`}
      {...rest}
    >
      <svg
        width={size * 0.86}
        height={size * 0.86}
        viewBox={`0 0 ${16 * cell} ${16 * cell}`}
        shapeRendering="crispEdges"
      >
        {grid16.map((row, y) =>
          row.map((ch, x) => {
            const fill = colorFor(ch);
            if (!fill) return null;
            return (
              <rect
                key={`${x}-${y}`}
                x={x * cell}
                y={y * cell}
                width={cell}
                height={cell}
                fill={fill}
              />
            );
          }),
        )}
        {accessories[role].map((a, i) => renderAccessory(a, `acc-${i}`, cell))}
      </svg>
    </div>
  );
};

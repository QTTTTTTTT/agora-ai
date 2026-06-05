// csvExport.ts — client-side CSV export utility.
//
// WHY THIS EXISTS
// ---------------
// AuditLog has a server-side CSV export endpoint
// (`exportFundAuditLogsCSV`); other list pages (TradeHistory,
// MemoryCenter, DecisionCenter, ABTestCompare, AgentLearning)
// don't. Building a server-side CSV endpoint per page is heavy
// (auth, pagination semantics, format negotiation) and slow to
// land. Most users don't need a 1M-row export — they want
// "what's in the table I'm looking at, but in Excel". Pure
// client-side CSV serialisation covers that case in 60-line
// utility, requires no backend change, and stays in lockstep
// with whatever the page chooses to display.
//
// FORMAT
// ------
//   - Standard RFC 4180-ish CSV: comma-separated, double-quote
//     wrapping for fields containing commas / newlines / quotes,
//     internal quotes escaped by doubling ("...").
//   - UTF-8 with a BOM prefix (\ufeff) so Excel for Windows
//     opens it as Unicode without prompting for encoding —
//     non-BOM CSV from a web app is a notorious "Chinese chars
//     show as ???" footgun for finance users.
//   - Cells that are null/undefined render as empty string.
//   - Numbers / booleans / Dates are stringified naturally;
//     Date objects use ISO format (toISOString) so spreadsheets
//     parse them as dates by default.
//
// API
// ---
//
//   import { exportRowsAsCsv } from "../lib/csvExport";
//
//   exportRowsAsCsv({
//     filename: "trade-history.csv",
//     columns: [
//       { key: "executedAt", header: "Executed at" },
//       { key: "symbol", header: "Symbol" },
//       { key: "side", header: "Side" },
//       { key: "quantity", header: "Qty" },
//       { key: "filledPrice", header: "Filled price",
//         format: (v) => typeof v === "number" ? v.toFixed(4) : "" },
//     ],
//     rows: trades,
//   });
//
// The columns array is the explicit header schema — we don't
// infer from the first row's keys because page authors usually
// want a SUBSET of fields with friendly headers, not the full
// API shape including server-internal IDs.
//
// SAFETY
// ------
// The "= cell injection" attack: a cell value starting with
// `=` `+` `-` `@` `\t` `\r` is interpreted by Excel as a
// formula. We prefix any such cell with a single quote `'` to
// neutralise it, since some user-generated fields (instrument
// names, agent labels, decision reasons) could legitimately
// start with `-` or `+`. Without this, a user importing an
// audit CSV could accidentally execute formulas baked into a
// malicious tenant name.

export interface CsvColumn<T> {
  /** Field key on each row. */
  key: keyof T | string;
  /** Header label written to the first row. */
  header: string;
  /** Optional formatter — receives the raw cell, returns the string to write. */
  format?: (value: unknown, row: T) => string;
}

export interface ExportRowsAsCsvOptions<T> {
  filename: string;
  columns: CsvColumn<T>[];
  rows: T[];
  /** Skip the UTF-8 BOM. Default false (BOM included). */
  noBom?: boolean;
}

/**
 * Serialise rows to a CSV string. Pure — no side effects, useful
 * for tests / non-browser environments.
 */
export function rowsToCsv<T>(opts: Omit<ExportRowsAsCsvOptions<T>, "filename">): string {
  const headerLine = opts.columns.map((c) => escapeCell(c.header)).join(",");
  const rowLines = opts.rows.map((row) =>
    opts.columns
      .map((col) => {
        const raw = (row as Record<string, unknown>)[col.key as string];
        const formatted = col.format ? col.format(raw, row) : stringifyCell(raw);
        return escapeCell(formatted);
      })
      .join(","),
  );
  const body = [headerLine, ...rowLines].join("\r\n"); // \r\n is the RFC default; Excel prefers it
  return opts.noBom ? body : "\ufeff" + body;
}

/**
 * Trigger a browser download of the rows as a .csv file. Returns
 * the produced CSV string for callers that also want to copy it
 * to clipboard or pipe somewhere else.
 */
export function exportRowsAsCsv<T>(opts: ExportRowsAsCsvOptions<T>): string {
  const csv = rowsToCsv({ columns: opts.columns, rows: opts.rows, noBom: opts.noBom });
  if (typeof window !== "undefined" && typeof document !== "undefined") {
    const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = opts.filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
    // Defer revoke so Safari has time to start the download. The
    // 1s window is conservative — most browsers fire 'click' and
    // initiate the fetch synchronously, but the explicit revoke
    // below keeps memory clean for users who export large CSVs
    // repeatedly without leaving the page.
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  return csv;
}

// ---------------------------------------------------------------
// Internal — cell stringification + CSV escaping.
// ---------------------------------------------------------------

function stringifyCell(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (value instanceof Date) return value.toISOString();
  if (typeof value === "boolean") return value ? "true" : "false";
  if (typeof value === "number") {
    if (!Number.isFinite(value)) return ""; // Inf / NaN render empty
    return String(value);
  }
  if (typeof value === "object") {
    // Pretty-print JSON objects compactly. Note: callers usually
    // prefer to format() these explicitly.
    try {
      return JSON.stringify(value);
    } catch {
      return "[object]";
    }
  }
  return String(value);
}

const FORMULA_INJECTION_LEAD = /^[=+\-@\t\r]/;

function escapeCell(value: string): string {
  // Defang formula-injection attempts by prefixing with a single
  // quote — Excel strips it on display but no longer interprets
  // the leading `=` etc as a formula.
  let v = value;
  if (FORMULA_INJECTION_LEAD.test(v)) v = "'" + v;

  // Quote-wrap if the cell contains comma / quote / CR / LF.
  if (/[",\r\n]/.test(v)) {
    v = '"' + v.replace(/"/g, '""') + '"';
  }
  return v;
}

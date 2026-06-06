// Translations for MemoryCenter.tsx — English (en-US).
//
// W10-3 — migrated from the inline `copy = useMemo(() =>
// language === "en-US" ? {...} : {...}, [language])` block.
// Mirrors the previous shape one-for-one so the consumer keeps
// using `copy.layerLabels.long_term`, `copy.viewModes[viewMode]`
// etc. unchanged. Nested objects keyed by typed enums
// (MemoryLayer / MemoryFocus / ViewMode) preserve their key
// sets verbatim — the parity guard
// (web/test/i18nNamespaceParity.test.ts) enforces that the
// en-US and zh-CN bundles list the same keys.
const memoryCenter = {
  loading: "Loading memory center...",
  retry: "Retry",
  missingFundId: "Missing fundId",
  loadFailed: "Failed to load memory center",
  title: "Memory center",
  subtitle:
    "Browse long-term memory, daily logs, distilled insights, and collaborative records for the fund, then filter by member, timeline, and statistics.",
  memberFilter: "Member filter",
  allMembers: "All members",
  focusLabel: "Focus",
  focusOptions: {
    all: "All memory",
    market: "Market research",
  },
  marketCoverage: "Market coverage",
  marketCoverageSubtitle:
    "Track how much of the team memory is now grounded in quotes, news, and research snapshots.",
  marketEntries: "Market entries",
  marketTags: "Market tags",
  latestMarketEntry: "Latest market entry",
  noMarketEntry: "None yet",
  researchBadge: "Research",
  newsBadge: "News",
  quoteBadge: "Quote",
  signalBadge: "Signal",
  viewModes: {
    content: "Content",
    search: "Search",
    timeline: "Timeline",
    stats: "Stats",
  },
  statsEmptyTitle: "No memory data to analyze yet",
  statsEmptyDescription:
    "Once workflows, research discussions, or team collaboration produce records, this view will summarize layer distribution, active members, and key themes.",
  totalEntries: "Total entries",
  mostActiveMember: "Most active member",
  layerDistribution: "Layer distribution",
  topTags: "Top tags",
  noTags: "No tags yet",
  searchPlaceholder: "Search memory by title or content...",
  foundResults: "results found",
  noSearchResults:
    "No matching memory found. Try different keywords or switch back to content view.",
  timelineTitle: "Daily log timeline",
  timelineEmpty:
    "No daily memory exists yet. Once day-to-day runs and collaboration records appear, key events will show up here in order.",
  contentEmpty:
    "This layer has no memory entries yet. Related collaboration records will settle here automatically later.",
  noSelectedEntry:
    "There is no memory to display in this layer yet. Switch to another layer or wait for new records.",
  memberPrefix: "Member:",
  unassignedMember: "Unassigned member",
  noActiveMember: "None yet",
  layerLabels: {
    long_term: "Long-term memory",
    daily: "Daily logs",
    dreams: "Insights",
    agent: "Collaboration memory",
    analysis: "Market analysis",
  },
  layerIcons: {
    long_term: "🧠",
    daily: "📅",
    dreams: "💭",
    agent: "🤖",
    analysis: "📈",
  },
} as const;

export default memoryCenter;

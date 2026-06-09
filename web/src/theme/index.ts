// theme/index.ts — central re-export for the cream/sage/ink design
// system. Pages and feature components should import from here
// (`import { Card, PillTag } from "../theme"`) rather than digging
// into individual files, so a future palette swap is a one-line
// rewrite of this barrel.

export { EnvelopeCard, EnvelopeSection } from "./EnvelopeCard";
export { PillTag, PillTagTone } from "./PillTag";
export type { PillTagToneName } from "./PillTag";
export { BlackPillButton, GhostPillButton } from "./BlackPillButton";
export { MascotAvatar } from "./MascotAvatar";
export type { MascotRole } from "./MascotAvatar";
export { TabPills } from "./TabPills";
export { MetricBlock } from "./MetricBlock";
export { SectionLabel } from "./SectionLabel";

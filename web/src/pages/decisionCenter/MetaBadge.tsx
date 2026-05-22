import React from "react";

export function MetaBadge({ children }: { children: React.ReactNode }) {
  return <span className="rounded-full bg-gray-100 px-2 py-1 text-xs text-gray-600">{children}</span>;
}

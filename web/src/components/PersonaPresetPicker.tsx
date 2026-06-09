import React from "react";
import type { AdvisorPreset } from "../lib/api";

// PersonaPresetPicker renders the 6+1 persona preset cards
// surfaced by `GET /api/advisor/presets`. The selected card
// becomes the preset_key on the subsequent /consult call.
//
// Visual: a responsive grid of cards, each showing the human
// label, a one-line description, and the comma-separated list of
// master / tactic keys the preset will run. The currently-selected
// card gets a ring + slightly elevated shadow.

export interface PersonaPresetPickerProps {
  presets: AdvisorPreset[];
  selectedKey: string | null;
  onSelect: (key: string) => void;
  language: string;
  disabled?: boolean;
}

const PersonaPresetPicker: React.FC<PersonaPresetPickerProps> = ({
  presets,
  selectedKey,
  onSelect,
  language,
  disabled,
}) => {
  if (presets.length === 0) {
    return (
      <div className="rounded-md border border-dashed border-slate-200 p-6 text-center text-sm text-slate-500">
        {language === "en-US" ? "No presets configured." : "暂无可用风格。"}
      </div>
    );
  }
  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {presets.map((p) => {
        const selected = selectedKey === p.preset_key;
        const label = language === "en-US" ? p.label_en : p.label_zh;
        const description = language === "en-US" ? p.description_en : p.description_zh;
        const agentList = [...p.master_keys, ...p.tactic_keys];
        const isTacticOnly = p.kind === "tactics";
        return (
          <button
            type="button"
            key={p.preset_key}
            disabled={disabled}
            onClick={() => onSelect(p.preset_key)}
            className={[
              "flex flex-col items-start gap-2 rounded-xl border p-4 text-left transition",
              selected
                ? "border-indigo-500 bg-indigo-50 ring-2 ring-indigo-300"
                : "border-slate-200 bg-white hover:border-indigo-300 hover:shadow-sm",
              disabled ? "cursor-not-allowed opacity-50" : "cursor-pointer",
            ].join(" ")}
          >
            <div className="flex w-full items-center justify-between">
              <span className="text-sm font-semibold text-slate-900">{label}</span>
              {isTacticOnly ? (
                <span className="rounded-full bg-amber-50 px-2 py-0.5 text-[10px] font-medium text-amber-700">
                  {language === "en-US" ? "A-share short" : "A 股短线"}
                </span>
              ) : null}
            </div>
            <p className="text-xs leading-relaxed text-slate-600">{description}</p>
            {agentList.length > 0 ? (
              <div className="mt-1 flex flex-wrap gap-1">
                {agentList.map((a) => (
                  <span
                    key={a}
                    className="rounded-full bg-slate-100 px-2 py-0.5 text-[10px] font-medium text-slate-600"
                  >
                    {a}
                  </span>
                ))}
              </div>
            ) : (
              <span className="text-[10px] italic text-slate-400">
                {language === "en-US" ? "Pick your own masters/tactics" : "由你自由选择大师/战法"}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
};

export default PersonaPresetPicker;

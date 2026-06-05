// validation.ts (en-US) — shared field-level validation messages.
// Used by the `validators` factory in web/src/lib/useFieldValidation.ts.
// Each message is intentionally short so it fits in a single inline
// hint below an input on mobile widths.

const validation = {
  required: "Required",
  invalidEmail: "Please enter a valid email address",
  minLength: "At least {{n}} characters",
  mustMatch: "Values don't match",
  passwordLetter: "Must include at least one letter",
  passwordDigit: "Must include at least one digit",
} as const;

export default validation;
export type ValidationCopy = typeof validation;

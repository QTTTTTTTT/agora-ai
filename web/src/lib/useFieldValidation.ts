// useFieldValidation.ts — onBlur-aware field-level validation.
//
// WHY THIS EXISTS
// ---------------
// The codebase's auth and settings forms (ForgotPassword, ResetPassword,
// VerifyEmail, Login register-tab, AccountSecurity, KYC, FundSettings,
// ModelConfig, Subscription cancel reason …) all share the same shape:
//
//   const [value, setValue] = useState("");
//   const [error, setError] = useState<string|null>(null);
//   async function handleSubmit() {
//     if (!validate(value)) { setError("invalid"); return; }
//     ... call API ...
//   }
//
// The validation only fires on submit. Users type a wrong-format value,
// click submit, see the error, fix the value, click submit again. That's
// two round-trips of frustration per typo. Real-world users on
// touch-screens with autocomplete typo emails twice as often as desktop
// keyboard users — they're the most affected.
//
// This hook adds onBlur-time validation:
//   1. Field's error is computed from the validator on every render.
//   2. The error is only DISPLAYED after the field has been touched
//      (blurred at least once). This avoids screaming red errors at
//      a user the moment they tab into an empty field.
//   3. On submit, the form's submit handler calls `markTouched()` to
//      surface any pending errors before running the API call.
//
// USAGE PATTERN
// -------------
//
//   const emailField = useFieldValidation(email, validators.email(t));
//
//   <input
//     value={email}
//     onChange={(e) => setEmail(e.target.value)}
//     onBlur={emailField.onBlur}
//     aria-invalid={Boolean(emailField.showError)}
//     aria-describedby={emailField.showError ? "email-error" : undefined}
//   />
//   {emailField.showError ? (
//     <p id="email-error" className="text-red-300 text-xs mt-1">
//       {emailField.error}
//     </p>
//   ) : null}
//
//   async function handleSubmit() {
//     emailField.markTouched();
//     if (emailField.error) return;
//     ... call API ...
//   }
//
// COMPOSABILITY
// -------------
// Validators are plain `(value) => string | null` functions. Compose
// them with `composeValidators(a, b, c)` — first non-null wins.
//
// The shipped builtin validators cover the most common cases used in
// existing pages:
//   validators.required(t)
//   validators.email(t)
//   validators.minLength(n, t)
//   validators.matches(regex, message)
//   validators.equalTo(otherValue, t)        // confirm-password style
//   validators.password(t)                   // length + 1 letter + 1 digit
//
// Each accepts the i18n `t` function so error messages localise via
// the `validation` namespace (see web/src/i18n/locales/{en-US,zh-CN}/validation.ts).

import { useCallback, useState } from "react";

export type Validator<T> = (value: T) => string | null;

export interface FieldValidationResult {
  /** Current error string, regardless of touched state. Used by submit handlers. */
  error: string | null;
  /** True once the input has been blurred (or markTouched was called). */
  touched: boolean;
  /** error AND touched — the value to render in the UI. */
  showError: string | null;
  /** Wire to <input onBlur>. */
  onBlur: () => void;
  /** Imperatively mark touched, e.g. inside the form's submit handler. */
  markTouched: () => void;
  /** Reset touched state (e.g. after successful submit + form reset). */
  reset: () => void;
}

export function useFieldValidation<T>(
  value: T,
  validate: Validator<T>,
): FieldValidationResult {
  const [touched, setTouched] = useState(false);

  const error = validate(value);

  const onBlur = useCallback(() => setTouched(true), []);
  const markTouched = useCallback(() => setTouched(true), []);
  const reset = useCallback(() => setTouched(false), []);

  return {
    error,
    touched,
    showError: touched ? error : null,
    onBlur,
    markTouched,
    reset,
  };
}

// composeValidators runs validators in order; the first one to return
// a non-null error wins. Useful for stacking required + format checks
// without duplicating the null-return short-circuit.
export function composeValidators<T>(
  ...vs: Array<Validator<T>>
): Validator<T> {
  return (value: T) => {
    for (const v of vs) {
      const err = v(value);
      if (err) return err;
    }
    return null;
  };
}

// Shape-friendly factory for builtin validators. Each returns a
// Validator<string> closed over the i18n `t` function. We accept `t`
// at validator-creation time so unit tests can pass a stub
// (e.g. `(k) => k`) without bringing the whole react-i18next runtime
// into a Jest harness.
//
// All keys live in the `validation` i18n namespace
// (web/src/i18n/locales/{en-US,zh-CN}/validation.ts).
export type TFunc = (key: string, opts?: Record<string, unknown>) => string;

export const validators = {
  required:
    (t: TFunc): Validator<string> =>
    (value) =>
      value && value.trim().length > 0 ? null : t("required"),

  email:
    (t: TFunc): Validator<string> =>
    (value) => {
      const trimmed = (value ?? "").trim();
      if (!trimmed) return null; // empty = no error here; pair with `required` if needed
      // The same regex our handlers use server-side, kept in sync to
      // avoid an "the form said yes but the API said no" experience.
      return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmed)
        ? null
        : t("invalidEmail");
    },

  minLength:
    (n: number, t: TFunc): Validator<string> =>
    (value) =>
      (value ?? "").length >= n ? null : t("minLength", { n }),

  matches:
    (regex: RegExp, message: string): Validator<string> =>
    (value) =>
      regex.test(value ?? "") ? null : message,

  equalTo:
    (otherValue: string, t: TFunc): Validator<string> =>
    (value) =>
      value === otherValue ? null : t("mustMatch"),

  password:
    (t: TFunc): Validator<string> =>
    (value) => {
      const v = value ?? "";
      if (v.length < 8) return t("minLength", { n: 8 });
      if (!/[A-Za-z]/.test(v)) return t("passwordLetter");
      if (!/[0-9]/.test(v)) return t("passwordDigit");
      return null;
    },
};

import React, { ReactNode, createContext, useCallback, useContext, useMemo, useState } from "react";

export type AppLanguage = "zh-CN" | "en-US";
export type DisplayCurrency = "USD" | "CNY";

const LANGUAGE_STORAGE_KEY = "fundai.language";
const DISPLAY_CURRENCY_STORAGE_KEY = "fundai.display_currency";
const USD_TO_CNY_RATE = 7.2;

interface PreferencesContextValue {
  language: AppLanguage;
  setLanguage: (value: AppLanguage) => void;
  displayCurrency: DisplayCurrency;
  setDisplayCurrency: (value: DisplayCurrency) => void;
}

const PreferencesContext = createContext<PreferencesContextValue | null>(null);

function detectLanguage(): AppLanguage {
  if (typeof window === "undefined") {
    return "zh-CN";
  }
  const stored = window.localStorage.getItem(LANGUAGE_STORAGE_KEY);
  if (stored === "zh-CN" || stored === "en-US") {
    return stored;
  }
  return window.navigator.language.toLowerCase().startsWith("en") ? "en-US" : "zh-CN";
}

function detectDisplayCurrency(): DisplayCurrency {
  if (typeof window === "undefined") {
    return "USD";
  }
  const stored = window.localStorage.getItem(DISPLAY_CURRENCY_STORAGE_KEY);
  if (stored === "USD" || stored === "CNY") {
    return stored;
  }
  return "USD";
}

export const PreferencesProvider: React.FC<{ children: ReactNode }> = ({ children }) => {
  const [languageState, setLanguageState] = useState<AppLanguage>(detectLanguage);
  const [displayCurrencyState, setDisplayCurrencyState] = useState<DisplayCurrency>(detectDisplayCurrency);

  const setLanguage = useCallback((value: AppLanguage) => {
    setLanguageState(value);
    if (typeof window !== "undefined") {
      window.localStorage.setItem(LANGUAGE_STORAGE_KEY, value);
    }
  }, []);

  const setDisplayCurrency = useCallback((value: DisplayCurrency) => {
    setDisplayCurrencyState(value);
    if (typeof window !== "undefined") {
      window.localStorage.setItem(DISPLAY_CURRENCY_STORAGE_KEY, value);
    }
  }, []);

  const contextValue = useMemo(
    () => ({
      language: languageState,
      setLanguage,
      displayCurrency: displayCurrencyState,
      setDisplayCurrency,
    }),
    [displayCurrencyState, languageState, setDisplayCurrency, setLanguage],
  );

  return <PreferencesContext.Provider value={contextValue}>{children}</PreferencesContext.Provider>;
};

export function useAppPreferences(): PreferencesContextValue {
  const context = useContext(PreferencesContext);
  if (!context) {
    throw new Error("useAppPreferences must be used within PreferencesProvider");
  }
  return context;
}

export function localeForLanguage(language: AppLanguage): string {
  return language === "en-US" ? "en-US" : "zh-CN";
}

export function formatDateTimeForLanguage(value: string | undefined, language: AppLanguage): string {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString(localeForLanguage(language), {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function formatDateValueForLanguage(
  value: string | Date | undefined,
  language: AppLanguage,
  options?: Intl.DateTimeFormatOptions,
): string {
  if (!value) {
    return "-";
  }
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) {
    return typeof value === "string" ? value : "-";
  }
  return date.toLocaleDateString(
    localeForLanguage(language),
    options ?? {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    },
  );
}

export function formatDateForLanguage(value: string | undefined, language: AppLanguage): string {
  return formatDateValueForLanguage(value, language);
}

export function formatNumberForLanguage(
  value: number,
  language: AppLanguage,
  options?: Intl.NumberFormatOptions,
): string {
  return value.toLocaleString(localeForLanguage(language), options);
}

function formatCurrencyAmount(amount: number, currency: string, language: AppLanguage): string {
  try {
    return amount.toLocaleString(localeForLanguage(language), { style: "currency", currency });
  } catch {
    return `${currency} ${amount.toLocaleString(localeForLanguage(language), {
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    })}`;
  }
}

export function convertMoneyForDisplay(
  value: number,
  baseCurrency: string | undefined,
  displayCurrency: DisplayCurrency,
): { amount: number; currency: string } {
  const normalizedBase = (baseCurrency ?? "USD").trim().toUpperCase() || "USD";
  let amount = value;
  let currency = normalizedBase;

  if (normalizedBase === displayCurrency) {
    currency = displayCurrency;
  } else if (normalizedBase === "USD" && displayCurrency === "CNY") {
    amount *= USD_TO_CNY_RATE;
    currency = "CNY";
  } else if (normalizedBase === "CNY" && displayCurrency === "USD") {
    amount /= USD_TO_CNY_RATE;
    currency = "USD";
  }

  return { amount, currency };
}

export function formatMoneyMinorForDisplay(
  valueMinor: number,
  baseCurrency: string | undefined,
  displayCurrency: DisplayCurrency,
  language: AppLanguage,
): string {
  const converted = convertMoneyForDisplay(valueMinor / 100, baseCurrency, displayCurrency);
  return formatCurrencyAmount(converted.amount, converted.currency, language);
}

export function formatMoneyForLanguage(value: number, currency: string | undefined, language: AppLanguage): string {
  const normalizedCurrency = (currency ?? "USD").trim().toUpperCase() || "USD";
  return formatCurrencyAmount(value, normalizedCurrency, language);
}

export function formatMoneyForDisplay(
  value: number,
  baseCurrency: string | undefined,
  displayCurrency: DisplayCurrency,
  language: AppLanguage,
): string {
  const converted = convertMoneyForDisplay(value, baseCurrency, displayCurrency);
  return formatCurrencyAmount(converted.amount, converted.currency, language);
}

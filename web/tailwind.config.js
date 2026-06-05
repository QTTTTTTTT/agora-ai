/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  // Class-based dark mode: Tailwind's `dark:` variant matches when
  // a `dark` class is present on a parent (we apply it to <html>
  // from the ThemeProvider). The alternative `media` strategy
  // would only follow `prefers-color-scheme` system-wide, which
  // doesn't let users pick "force light" or "force dark"
  // independent of OS — and that's a feature most users want,
  // especially investors who run alongside a chart-tool that
  // disagrees with their OS setting. The `system` mode in our
  // own ThemeProvider falls back to `prefers-color-scheme`.
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        brand: {
          50:  '#eef2ff',
          100: '#e0e7ff',
          200: '#c7d2fe',
          300: '#a5b4fc',
          400: '#818cf8',
          500: '#6366f1',
          600: '#4f46e5',
          700: '#4338ca',
          800: '#3730a3',
          900: '#312e81',
        },
      },
      fontFamily: {
        sans: ['Inter', 'system-ui', '-apple-system', 'sans-serif'],
        mono: ['JetBrains Mono', 'Fira Code', 'monospace'],
      },
      // Indeterminate progress-bar animation used by the route-level
      // <Suspense> fallback (components/RouteFallback.tsx). Tailwind's
      // built-in animations cover spin/ping/pulse/bounce but not the
      // sliding-bar pattern users expect for "page is loading" — we
      // keep it as a named utility so other suspense boundaries (e.g.
      // FundLayout's nested routes) can reuse it without duplicating
      // CSS keyframes inline.
      keyframes: {
        'progress-slide': {
          '0%':   { transform: 'translateX(-100%)' },
          '100%': { transform: 'translateX(400%)' },
        },
      },
      animation: {
        'progress-slide': 'progress-slide 1.2s ease-in-out infinite',
      },
    },
  },
  plugins: [],
}

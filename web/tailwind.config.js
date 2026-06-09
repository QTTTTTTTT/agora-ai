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
        // Brand pastel palette — the light cream / sage green /
        // black-pill aesthetic from the design refresh. The
        // canonical body background is `cream-50` and primary
        // surfaces are `cream-0` (true white) so cards "lift"
        // off the page without a heavy shadow.
        cream: {
          0:   '#ffffff',
          50:  '#f4f2ee', // page bg, soft envelope paper
          100: '#ebe8e1',
          200: '#dcd8cf',
        },
        // Sage / mint green — the ambient accent that washes
        // the page and tints the "ready" / positive chips.
        sage: {
          50:  '#eef5ec',
          100: '#dcebd6',
          200: '#bedabb',
          300: '#9ec99a',
          400: '#73b272',
          500: '#1faa64', // primary positive number color
          600: '#168a52',
          700: '#0f6a3f',
        },
        // Coral / hint orange — the "needs attention" chip
        // and the small accent underline on tab indicators.
        coral: {
          50:  '#fdf0e7',
          100: '#fbe1d1',
          200: '#f7c4a3',
          300: '#f7a05d',
          400: '#f08a40',
          500: '#dc6f24',
        },
        // Risk red — the 风控优先 chip + critical risk row.
        risk: {
          50:  '#fdebec',
          100: '#fad1d3',
          200: '#f4a3a8',
          300: '#ee7079',
          400: '#e64949', // primary risk red
          500: '#c5343a',
        },
        // Ink — the deep black used by primary CTA pills.
        ink: {
          50:  '#f6f5f2',
          100: '#e5e3dd',
          200: '#c5c1b6',
          300: '#7a766c',
          500: '#3a382f',
          700: '#1f1d18',
          900: '#111110', // primary CTA fill
        },
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
        // The cream-card aesthetic looks best with a slightly
        // friendlier sans (Manrope/MiSans) when available, and
        // Inter as the universal fallback.
        sans: [
          'Manrope',
          'MiSans',
          '"PingFang SC"',
          '"Hiragino Sans GB"',
          'Inter',
          'system-ui',
          '-apple-system',
          'sans-serif',
        ],
        // Pixel-art mascot captions and 像素 badges. Falls back
        // to monospace stack if "Press Start 2P" isn't loaded.
        pixel: [
          '"Press Start 2P"',
          '"VT323"',
          'ui-monospace',
          'SFMono-Regular',
          'monospace',
        ],
        mono: ['JetBrains Mono', 'Fira Code', 'monospace'],
      },
      borderRadius: {
        '2.5xl': '1.25rem',
        // The signature "envelope" card radius used across the
        // refresh: 24px on cards, 999px on pills.
        'envelope': '1.5rem',
        'envelope-lg': '1.75rem',
      },
      boxShadow: {
        // Card shadows in the refresh are extremely soft — almost
        // a tinted "lift" rather than a drop. These match the
        // reference screenshots more faithfully than tw's default
        // shadow scale (which is far too harsh on cream).
        'envelope':       '0 1px 2px rgba(28, 28, 24, 0.04), 0 6px 24px -12px rgba(28, 28, 24, 0.08)',
        'envelope-hover': '0 2px 4px rgba(28, 28, 24, 0.06), 0 12px 32px -14px rgba(28, 28, 24, 0.14)',
        'pill':           '0 1px 1px rgba(28, 28, 24, 0.04), 0 2px 8px -4px rgba(28, 28, 24, 0.10)',
        'pill-ink':       '0 4px 14px -6px rgba(17, 17, 16, 0.45)',
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
        'fade-up': {
          '0%':   { opacity: '0', transform: 'translateY(8px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        'mascot-bob': {
          '0%, 100%': { transform: 'translateY(0)' },
          '50%':      { transform: 'translateY(-2px)' },
        },
      },
      animation: {
        'progress-slide': 'progress-slide 1.2s ease-in-out infinite',
        'fade-up':        'fade-up 0.35s ease-out both',
        'mascot-bob':     'mascot-bob 3s ease-in-out infinite',
      },
    },
  },
  plugins: [],
}

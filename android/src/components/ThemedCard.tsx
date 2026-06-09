/**
 * ThemedCard, PillTag, BlackPillButton, MetricBlock — shared
 * styling primitives for the cream/sage/ink redesign on the
 * Android app. The visual language mirrors web's
 * `web/src/theme/*` so screens look coherent across platforms
 * without a deep RN UI library dependency.
 *
 * Each primitive reads the active theme via `useTheme()` so dark
 * mode users still get consistent depth + contrast (the cream
 * tokens swap to slate equivalents in `theme.tsx`).
 */

import React from 'react';
import { Pressable, StyleSheet, Text, View, ViewStyle } from 'react-native';
import type { TextStyle } from 'react-native';

import { useTheme } from '../lib/theme';

interface ThemedCardProps {
  children: React.ReactNode;
  /** Tighter padding for inline use inside grids. */
  compact?: boolean;
  /** Optional left-edge tone stripe, mirrors web EnvelopeCard. */
  tone?: 'neutral' | 'sage' | 'coral' | 'risk';
  style?: ViewStyle;
}

export function ThemedCard({ children, compact, tone = 'neutral', style }: ThemedCardProps): JSX.Element {
  const { colors } = useTheme();
  const stripeBg =
    tone === 'sage'  ? colors.sage  :
    tone === 'coral' ? colors.coral :
    tone === 'risk'  ? colors.risk  : null;
  return (
    <View
      style={[
        {
          backgroundColor: colors.surface,
          borderRadius: 24,
          paddingHorizontal: compact ? 14 : 18,
          paddingVertical: compact ? 14 : 20,
          marginBottom: 14,
          borderWidth: 1,
          borderColor: colors.border,
          shadowColor: '#1c1c18',
          shadowOpacity: 0.06,
          shadowRadius: 14,
          shadowOffset: { width: 0, height: 4 },
          elevation: 2,
        },
        style,
      ]}
    >
      {stripeBg ? (
        <View
          style={{
            position: 'absolute',
            left: 0,
            top: 22,
            bottom: 22,
            width: 4,
            borderTopRightRadius: 4,
            borderBottomRightRadius: 4,
            backgroundColor: stripeBg,
          }}
        />
      ) : null}
      {children}
    </View>
  );
}

interface PillTagProps {
  children: React.ReactNode;
  tone?: 'sage' | 'coral' | 'risk' | 'ink' | 'muted' | 'info';
  size?: 'sm' | 'md';
  dot?: boolean;
}

export function PillTag({ children, tone = 'muted', size = 'md', dot }: PillTagProps): JSX.Element {
  const { colors } = useTheme();
  const map = {
    sage:  { bg: colors.sage,    fg: colors.sageStrong },
    coral: { bg: colors.coral,   fg: colors.coralStrong },
    risk:  { bg: colors.risk,    fg: colors.riskStrong },
    ink:   { bg: colors.border,  fg: colors.text },
    muted: { bg: colors.surfaceAlt, fg: colors.textMuted },
    info:  { bg: '#dde7ff',      fg: '#4338ca' },
  } as const;
  const { bg, fg } = map[tone];
  const padV = size === 'sm' ? 2 : 4;
  const padH = size === 'sm' ? 8 : 12;
  const fontSize = size === 'sm' ? 11 : 12;
  return (
    <View
      style={{
        backgroundColor: bg,
        borderRadius: 999,
        paddingVertical: padV,
        paddingHorizontal: padH,
        alignSelf: 'flex-start',
        flexDirection: 'row',
        alignItems: 'center',
      }}
    >
      {dot ? (
        <View style={{ width: 6, height: 6, borderRadius: 3, backgroundColor: fg, opacity: 0.7, marginRight: 6 }} />
      ) : null}
      <Text style={{ color: fg, fontSize, fontWeight: '600' }}>{children}</Text>
    </View>
  );
}

interface BlackPillButtonProps {
  label: string;
  onPress: () => void;
  variant?: 'ink' | 'ghost' | 'sage';
  withArrow?: boolean;
  size?: 'sm' | 'md' | 'lg';
  block?: boolean;
  disabled?: boolean;
  accessibilityLabel?: string;
  style?: ViewStyle;
}

export function BlackPillButton({
  label,
  onPress,
  variant = 'ink',
  withArrow,
  size = 'md',
  block,
  disabled,
  accessibilityLabel,
  style,
}: BlackPillButtonProps): JSX.Element {
  const { colors } = useTheme();
  const heights: Record<NonNullable<BlackPillButtonProps['size']>, number> = { sm: 36, md: 44, lg: 52 };
  const padHs: Record<NonNullable<BlackPillButtonProps['size']>, number> = { sm: 16, md: 20, lg: 24 };

  const palette: Record<NonNullable<BlackPillButtonProps['variant']>, { bg: string; fg: string; border?: string }> = {
    ink: { bg: colors.ink, fg: '#ffffff' },
    sage: { bg: colors.sage, fg: colors.sageStrong },
    ghost: { bg: colors.surface, fg: colors.text, border: colors.border },
  };
  const { bg, fg, border } = palette[variant];

  return (
    <Pressable
      onPress={disabled ? undefined : onPress}
      accessibilityRole="button"
      accessibilityLabel={accessibilityLabel ?? label}
      accessibilityState={{ disabled: !!disabled }}
      style={[
        {
          height: heights[size],
          borderRadius: 999,
          paddingHorizontal: padHs[size],
          backgroundColor: bg,
          borderWidth: border ? 1 : 0,
          borderColor: border,
          flexDirection: 'row',
          alignItems: 'center',
          justifyContent: 'center',
          alignSelf: block ? 'stretch' : 'flex-start',
          opacity: disabled ? 0.4 : 1,
          shadowColor: variant === 'ink' ? '#11110d' : '#1c1c18',
          shadowOpacity: variant === 'ink' ? 0.35 : 0.08,
          shadowRadius: variant === 'ink' ? 14 : 6,
          shadowOffset: { width: 0, height: 4 },
          elevation: variant === 'ink' ? 4 : 1,
        },
        style,
      ]}
    >
      <Text style={{ color: fg, fontWeight: '700', fontSize: 14 }}>{label}</Text>
      {withArrow ? (
        <Text style={{ color: fg, fontWeight: '700', fontSize: 16, marginLeft: 8 }} accessibilityElementsHidden>
          →
        </Text>
      ) : null}
    </Pressable>
  );
}

interface MetricBlockProps {
  label: string;
  value: string;
  tone?: 'neutral' | 'positive' | 'negative';
  hint?: string;
}

export function MetricBlock({ label, value, tone = 'neutral', hint }: MetricBlockProps): JSX.Element {
  const { colors } = useTheme();
  const valueColor =
    tone === 'positive' ? colors.success :
    tone === 'negative' ? colors.danger  : colors.text;
  return (
    <View style={{ minWidth: 0, paddingVertical: 4 }}>
      <Text style={{ color: colors.textMuted, fontSize: 11, fontWeight: '500' }}>{label}</Text>
      <Text style={{ color: valueColor, fontSize: 22, fontWeight: '800', marginTop: 4 } as TextStyle}>{value}</Text>
      {hint ? <Text style={{ color: colors.textMuted, fontSize: 11, marginTop: 2 }}>{hint}</Text> : null}
    </View>
  );
}

interface SectionLabelProps {
  children: React.ReactNode;
  trailing?: React.ReactNode;
  style?: ViewStyle;
}

export function SectionLabel({ children, trailing, style }: SectionLabelProps): JSX.Element {
  const { colors } = useTheme();
  return (
    <View
      style={[
        { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', paddingHorizontal: 4, marginBottom: 8 },
        style,
      ]}
    >
      <Text style={{ color: colors.textMuted, fontSize: 11, fontWeight: '700', letterSpacing: 1.5 }}>
        {String(children).toUpperCase()}
      </Text>
      {trailing ? (
        typeof trailing === 'string' ? (
          <Text style={{ color: colors.sageStrong, fontWeight: '700', fontSize: 13 }}>{trailing}</Text>
        ) : (
          trailing
        )
      ) : null}
    </View>
  );
}

// Tiny pixel-character avatar — RN flavor of the web MascotAvatar.
// Composes a 16×16 grid out of nested Views (RN doesn't ship inline
// SVG without an extra dep, and adding react-native-svg for one
// component is overkill). The grid resolution is intentionally
// coarse — we want the chunky retro look.
export type MascotRole = 'captain' | 'intel' | 'picker' | 'trader' | 'risk' | 'analyst';

interface MascotPalette {
  bg: string; hair: string; skin: string; uniform: string; accent: string; mouth: string;
}
const palettes: Record<MascotRole, MascotPalette> = {
  captain: { bg: '#f0ebe0', hair: '#1f1d18', skin: '#f3d2b1', uniform: '#1f3a52', accent: '#d4a75c', mouth: '#7a4a2e' },
  intel:   { bg: '#e6f0e8', hair: '#5b3b25', skin: '#f3d2b1', uniform: '#7da1a8', accent: '#3a6f78', mouth: '#7a4a2e' },
  picker:  { bg: '#f1ece2', hair: '#3b2418', skin: '#f3d2b1', uniform: '#7d6dc4', accent: '#3a3372', mouth: '#7a4a2e' },
  trader:  { bg: '#e9f1e2', hair: '#1f1d18', skin: '#f3d2b1', uniform: '#3a3a3a', accent: '#1faa64', mouth: '#7a4a2e' },
  risk:    { bg: '#f4e6e0', hair: '#1f1d18', skin: '#f3d2b1', uniform: '#3a3a3a', accent: '#c5343a', mouth: '#7a4a2e' },
  analyst: { bg: '#eef2ff', hair: '#3b2418', skin: '#f3d2b1', uniform: '#4a4a52', accent: '#6b6f7a', mouth: '#7a4a2e' },
};

const figure: string[] = [
  '................',
  '................',
  '....HHHHHHHH....',
  '...HHHHHHHHHH...',
  '..HSSSSSSSSSSH..',
  '..HSEESSSEESSH..',
  '..HSSSSSSSSSSH..',
  '..HSSSMMMMSSSH..',
  '...HSSSSSSSSH...',
  '...UUUUUUUUUU...',
  '..UUAAAAAAAAUU..',
  '.UUAAAAAAAAAAUU.',
  '.UAAAAAAAAAAAAU.',
  '.UAAAAAAAAAAAAU.',
  '.UUAAAAAAAAAAUU.',
  '..UUUUUUUUUUUU..',
];

interface MascotAvatarProps {
  role: MascotRole;
  size?: number;
  style?: ViewStyle;
}

export function MascotAvatar({ role, size = 72, style }: MascotAvatarProps): JSX.Element {
  const palette = palettes[role];
  const grid = 16;
  const cell = (size * 0.86) / grid;
  const innerSize = cell * grid;

  function colorFor(ch: string): string | null {
    switch (ch) {
      case 'H': return palette.hair;
      case 'S': return palette.skin;
      case 'U': return palette.uniform;
      case 'A': return palette.accent;
      case 'M': return palette.mouth;
      case 'E': return '#1a1a1a';
      default:  return null;
    }
  }

  return (
    <View
      style={[
        {
          width: size,
          height: size,
          borderRadius: 18,
          backgroundColor: palette.bg,
          alignItems: 'center',
          justifyContent: 'center',
        },
        style,
      ]}
    >
      <View style={{ width: innerSize, height: innerSize }}>
        {figure.map((row, y) => (
          <View key={`row-${y}`} style={{ flexDirection: 'row' }}>
            {row.split('').map((ch, x) => {
              const fill = colorFor(ch);
              return (
                <View
                  key={`${x}-${y}`}
                  style={{
                    width: cell,
                    height: cell,
                    backgroundColor: fill ?? 'transparent',
                  }}
                />
              );
            })}
          </View>
        ))}
      </View>
    </View>
  );
}

// Avoid an "no exported" warning if we tweak imports later.
const _styles = StyleSheet.create({});
export default _styles;

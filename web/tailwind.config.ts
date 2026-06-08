import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      fontFamily: {
        sans: [
          'Geist Variable',
          'Geist',
          'ui-sans-serif',
          'system-ui',
          '-apple-system',
          'BlinkMacSystemFont',
          '"Segoe UI"',
          'Roboto',
          'sans-serif',
        ],
        head: [
          'Geist Variable',
          'Geist',
          'ui-sans-serif',
          'system-ui',
          'sans-serif',
        ],
        mono: [
          'Geist Mono Variable',
          'Geist Mono',
          'ui-monospace',
          'SFMono-Regular',
          'Menlo',
          'Monaco',
          'Consolas',
          'monospace',
        ],
      },
      borderRadius: {
        DEFAULT: 'var(--radius-md)',
        sm: 'var(--radius-sm)',
        md: 'var(--radius-md)',
        lg: 'var(--radius-lg)',
        xl: 'var(--radius-xl)',
        pill: 'var(--radius-pill)',
      },
      fontSize: {
        /* Minimum UI size: 14px (0.875rem). xs matches sm — nothing smaller in the product. */
        xs: ['0.875rem', { lineHeight: '1.5' }],
        '2xs': ['0.875rem', { lineHeight: '1.5', letterSpacing: '0.04em' }],
        caption: ['0.875rem', { lineHeight: '1.5', letterSpacing: '0.01em' }],
        overline: ['0.875rem', { lineHeight: '1.5', letterSpacing: '0.08em', fontWeight: '600' }],
        'mono-sm': ['0.875rem', { lineHeight: '1.5' }],
        /* Atrona type ramp — prefer font-semibold (600) on headings, not bold */
        display: ['3.5rem', { lineHeight: '1.05', letterSpacing: '-0.02em' }],
        h1: ['2.5rem', { lineHeight: '1.1', letterSpacing: '-0.02em' }],
        h2: ['1.875rem', { lineHeight: '1.15', letterSpacing: '-0.01em' }],
        h3: ['1.375rem', { lineHeight: '1.25', letterSpacing: '-0.01em' }],
        h4: ['1.125rem', { lineHeight: '1.3' }],
        'body-lg': ['1.125rem', { lineHeight: '1.6' }],
        body: ['1rem', { lineHeight: '1.6' }],
        'body-sm': ['0.875rem', { lineHeight: '1.5' }],
      },
      maxWidth: {
        content: '1280px',
      },
      boxShadow: {
        pop: 'var(--shadow-pop)',
        modal: 'var(--shadow-modal)',
      },
      colors: {
        'surface-0': 'rgb(var(--color-surface-0) / <alpha-value>)',
        'surface-1': 'rgb(var(--color-surface-1) / <alpha-value>)',
        'surface-2': 'rgb(var(--color-surface-2) / <alpha-value>)',
        'surface-3': 'rgb(var(--color-surface-3) / <alpha-value>)',
        primary: {
          DEFAULT: 'rgb(var(--color-primary) / <alpha-value>)',
          contrast: 'rgb(var(--color-primary-contrast) / <alpha-value>)',
        },
        brand: {
          DEFAULT: 'rgb(var(--color-brand) / <alpha-value>)',
          muted: 'var(--color-brand-muted)',
          strong: 'rgb(var(--color-brand-strong) / <alpha-value>)',
        },
        success: 'rgb(var(--color-success) / <alpha-value>)',
        warning: 'rgb(var(--color-warning) / <alpha-value>)',
        danger: 'rgb(var(--color-danger) / <alpha-value>)',
        'risk-orange': 'rgb(var(--color-risk-orange) / <alpha-value>)',
        'text-primary': 'rgb(var(--color-text-primary) / <alpha-value>)',
        'text-secondary': 'rgb(var(--color-text-secondary) / <alpha-value>)',
        'text-tertiary': 'rgb(var(--color-text-tertiary) / <alpha-value>)',
        'border-default': 'rgb(var(--color-border-default) / <alpha-value>)',
        'border-subtle': 'var(--color-border-subtle)',
        'border-strong': 'rgb(var(--color-border-strong) / <alpha-value>)',
        ring: 'rgb(var(--color-ring) / <alpha-value>)',
        info: {
          DEFAULT: 'rgb(var(--color-info) / <alpha-value>)',
          bg: 'var(--color-info-bg)',
        },
      },
    },
  },
  plugins: [],
} satisfies Config

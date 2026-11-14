/** Small stroke icons, drawn in the current text colour. Always decorative. */

const PATHS = {
  live: 'M12 10a2 2 0 1 1 0 4a2 2 0 1 1 0-4M8.5 8.5a5 5 0 0 0 0 7M15.5 8.5a5 5 0 0 1 0 7M5.6 5.6a9 9 0 0 0 0 12.8M18.4 5.6a9 9 0 0 1 0 12.8',
  dashboard: 'M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z',
  cameras: 'M4 6h9a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2zM15 10l7-4v12l-7-4',
  destinations:
    'M4 5c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3zM4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3',
  recordings: 'M12 3a9 9 0 1 1 0 18a9 9 0 1 1 0-18M10 8l6 4-6 4z',
  events: 'M4 6h16M4 12h16M4 18h10',
  system:
    'M12 9a3 3 0 1 1 0 6a3 3 0 1 1 0-6M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9L7 7M17 17l2.1 2.1M4.9 19.1L7 17M17 7l2.1-2.1',
  plus: 'M12 5v14M5 12h14',
  edit: 'M4 20h4L19 9l-4-4L4 16z',
  back: 'M15 6l-6 6 6 6',
  next: 'M9 6l6 6-6 6',
  fullscreen: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
  grid: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
  clock: 'M12 3a9 9 0 1 1 0 18a9 9 0 1 1 0-18M12 7v5l3 2',
  download: 'M12 4v11M7 10l5 5 5-5M5 20h14',
  lens: 'M12 4a8 8 0 1 1 0 16a8 8 0 1 1 0-16M12 9a3 3 0 1 1 0 6a3 3 0 1 1 0-6',
  refresh: 'M20 11a8 8 0 0 0-14.9-4M4 4v4h4M4 13a8 8 0 0 0 14.9 4M20 20v-4h-4',
} as const;

export type IconName = keyof typeof PATHS;

export function Icon({ name, size = 20 }: { name: IconName; size?: number }) {
  return (
    <svg
      className="icon"
      aria-hidden="true"
      focusable="false"
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d={PATHS[name]} />
    </svg>
  );
}

/** The ButterCams mark: a yellow square with a lens. */
export function BrandMark({ size = 36 }: { size?: number }) {
  return (
    <span className="brand-mark" style={{ width: size, height: size }} aria-hidden="true">
      <Icon name="lens" size={Math.round(size * 0.56)} />
    </span>
  );
}

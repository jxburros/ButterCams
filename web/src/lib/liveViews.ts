// Saved camera views for the Live page. They are kept in this browser's
// localStorage (docs/DECISIONS.md, "Live page views"): they hold no recording
// settings, so nothing in config.json or the API changes for them.

export type TileLayout = 1 | 4 | 9 | 16;

export const LAYOUTS: TileLayout[] = [1, 4, 9, 16];

export interface LiveView {
  id: string;
  name: string;
  /** Camera ids in tile order. Ignored for the built-in "All cameras" view. */
  cameraIds: string[];
  layout: TileLayout;
}

export const ALL_VIEW_ID = 'all';

interface Stored {
  views: LiveView[];
  allLayout: TileLayout;
  selected: string;
}

const KEY = 'buttercams.live.v1';

const DEFAULTS: Stored = { views: [], allLayout: 9, selected: ALL_VIEW_ID };

function isLayout(value: unknown): value is TileLayout {
  return value === 1 || value === 4 || value === 9 || value === 16;
}

function cleanView(value: unknown): LiveView | null {
  if (typeof value !== 'object' || value === null) return null;
  const v = value as Record<string, unknown>;
  if (typeof v.id !== 'string' || v.id === '' || v.id === ALL_VIEW_ID) return null;
  if (typeof v.name !== 'string') return null;
  const cameraIds = Array.isArray(v.cameraIds) ? v.cameraIds.filter((id): id is string => typeof id === 'string') : [];
  return { id: v.id, name: v.name.slice(0, 60), cameraIds, layout: isLayout(v.layout) ? v.layout : 4 };
}

export function loadLiveViews(): Stored {
  try {
    const raw = window.localStorage.getItem(KEY);
    if (raw === null) return DEFAULTS;
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    const views = Array.isArray(parsed.views)
      ? parsed.views.map(cleanView).filter((view): view is LiveView => view !== null)
      : [];
    return {
      views,
      allLayout: isLayout(parsed.allLayout) ? parsed.allLayout : DEFAULTS.allLayout,
      selected: typeof parsed.selected === 'string' ? parsed.selected : ALL_VIEW_ID,
    };
  } catch {
    return DEFAULTS;
  }
}

export function saveLiveViews(stored: Stored): void {
  try {
    window.localStorage.setItem(KEY, JSON.stringify(stored));
  } catch {
    // Private windows and full storage: the views last until the tab closes.
  }
}

export function newViewId(): string {
  return `view-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`;
}

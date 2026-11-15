// Saved camera views of the Live page (spec 16.6). The views themselves live
// on the server: every user has their own, and Admins also keep shared ones.
// What stays in this browser is small and personal to it: which view was
// open and the layout of the built-in "All cameras" view.
//
// v1 kept the views in localStorage. Those are offered once for import into
// the user's own views.

import type { LiveView, TileLayout } from '../api/types';

export type { LiveView, TileLayout };

export const LAYOUTS: TileLayout[] = [1, 4, 9, 16];

/** The server accepts at most this many views per list. */
export const MAX_VIEWS = 50;

export const ALL_VIEW_ID = 'all';

/** Where a view comes from. */
export type ViewScope = 'all' | 'mine' | 'shared';

/** The key of a view in the picker: "all", "mine:<id>" or "shared:<id>". */
export function viewKey(scope: ViewScope, id: string): string {
  return scope === 'all' ? ALL_VIEW_ID : `${scope}:${id}`;
}

export interface LivePrefs {
  allLayout: TileLayout;
  /** A `viewKey`. */
  selected: string;
}

const PREFS_KEY = 'buttercams.live.prefs.v2';
const LEGACY_KEY = 'buttercams.live.v1';
const LEGACY_DONE_KEY = 'buttercams.live.v1.imported';

const DEFAULT_PREFS: LivePrefs = { allLayout: 9, selected: ALL_VIEW_ID };

function isLayout(value: unknown): value is TileLayout {
  return value === 1 || value === 4 || value === 9 || value === 16;
}

function cleanView(value: unknown): LiveView | null {
  if (typeof value !== 'object' || value === null) return null;
  const v = value as Record<string, unknown>;
  if (typeof v.id !== 'string' || v.id === '' || v.id === ALL_VIEW_ID) return null;
  if (typeof v.name !== 'string' || v.name.trim() === '') return null;
  const cameraIds = Array.isArray(v.cameraIds) ? v.cameraIds.filter((id): id is string => typeof id === 'string') : [];
  return { id: v.id, name: v.name.trim().slice(0, 60), cameraIds: [...new Set(cameraIds)], layout: isLayout(v.layout) ? v.layout : 4 };
}

/** Reads what v1 stored: its views, and the preferences that carry over. */
export function parseLegacy(raw: string | null): { views: LiveView[]; prefs: LivePrefs } {
  if (raw === null) return { views: [], prefs: DEFAULT_PREFS };
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    const views = Array.isArray(parsed.views)
      ? parsed.views.map(cleanView).filter((view): view is LiveView => view !== null)
      : [];
    const selected = typeof parsed.selected === 'string' && parsed.selected !== ALL_VIEW_ID ? `mine:${parsed.selected}` : ALL_VIEW_ID;
    return { views, prefs: { allLayout: isLayout(parsed.allLayout) ? parsed.allLayout : DEFAULT_PREFS.allLayout, selected } };
  } catch {
    return { views: [], prefs: DEFAULT_PREFS };
  }
}

const ID_OK = /^[A-Za-z0-9_-]{1,64}$/;

export function newViewId(): string {
  return `view-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`;
}

/** An id like `wanted` that no view in `taken` uses and that the server accepts. */
export function freeViewId(wanted: string, taken: Iterable<string>, makeId: () => string = newViewId): string {
  const used = new Set(taken);
  if (ID_OK.test(wanted) && wanted !== ALL_VIEW_ID && !used.has(wanted)) return wanted;
  for (;;) {
    const id = makeId();
    if (!used.has(id)) return id;
  }
}

/**
 * Adds views from this browser (v1) to the user's own views on the server.
 * A view that is already there under the same name with the same cameras is
 * not added twice; ids are kept where possible.
 */
export function mergeImported(mine: LiveView[], legacy: LiveView[], makeId: () => string = newViewId): LiveView[] {
  const out = [...mine];
  for (const view of legacy) {
    if (out.length >= MAX_VIEWS) break;
    const same = out.some(
      (existing) =>
        existing.name === view.name &&
        existing.layout === view.layout &&
        existing.cameraIds.join('\n') === view.cameraIds.join('\n'),
    );
    if (same) continue;
    out.push({ ...view, id: freeViewId(view.id, out.map((v) => v.id), makeId) });
  }
  return out;
}

/** Puts a view into a list: replaces the one with the same id, or appends it. */
export function upsertView(views: LiveView[], view: LiveView): LiveView[] {
  return views.some((existing) => existing.id === view.id)
    ? views.map((existing) => (existing.id === view.id ? view : existing))
    : [...views, view];
}

/** A copy of one of the user's own views for the shared list. */
export function sharedCopy(view: LiveView, shared: LiveView[], makeId: () => string = newViewId): LiveView {
  return { ...view, id: freeViewId(view.id, shared.map((v) => v.id), makeId) };
}

// ------------------------------------------------------------- localStorage

function read(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

function write(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    // Private windows and full storage: it lasts until the tab closes.
  }
}

export function loadPrefs(): LivePrefs {
  const raw = read(PREFS_KEY);
  if (raw === null) return parseLegacy(read(LEGACY_KEY)).prefs;
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    return {
      allLayout: isLayout(parsed.allLayout) ? parsed.allLayout : DEFAULT_PREFS.allLayout,
      selected: typeof parsed.selected === 'string' ? parsed.selected : ALL_VIEW_ID,
    };
  } catch {
    return DEFAULT_PREFS;
  }
}

export function savePrefs(prefs: LivePrefs): void {
  write(PREFS_KEY, JSON.stringify(prefs));
}

/** The v1 views still waiting in this browser; empty once they were imported or declined. */
export function pendingLegacyViews(): LiveView[] {
  if (read(LEGACY_DONE_KEY) !== null) return [];
  return parseLegacy(read(LEGACY_KEY)).views;
}

/** Remembers that the import was offered and answered, so it is not offered again. */
export function legacyHandled(): void {
  write(LEGACY_DONE_KEY, new Date().toISOString());
}

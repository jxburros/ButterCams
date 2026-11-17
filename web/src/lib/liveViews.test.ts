import { describe, expect, it } from 'vitest';
import type { LiveView } from '../api/types';
import { freeViewId, mergeImported, parseLegacy, sharedCopy, upsertView, viewKey } from './liveViews';

const view = (id: string, name: string, cameraIds: string[] = ['a'], layout: 1 | 4 | 9 | 16 = 4): LiveView => ({
  id,
  name,
  cameraIds,
  layout,
});

let n = 0;
const makeId = () => `gen-${++n}`;

describe('views saved by v1 in this browser', () => {
  it('are read, with broken entries dropped', () => {
    const raw = JSON.stringify({
      allLayout: 16,
      selected: 'view-1',
      views: [
        { id: 'view-1', name: ' Entrances ', layout: 9, cameraIds: ['a', 'a', 7, 'b'] },
        { id: 'all', name: 'Reserved id', cameraIds: [] },
        { id: 'view-2', name: '   ' },
        { id: 'view-3', name: 'No layout' },
        'nonsense',
      ],
    });
    const { views, prefs } = parseLegacy(raw);
    expect(views).toEqual([view('view-1', 'Entrances', ['a', 'b'], 9), view('view-3', 'No layout', [], 4)]);
    expect(prefs).toEqual({ allLayout: 16, selected: 'mine:view-1' });
  });
  it('are empty for no or unreadable storage', () => {
    expect(parseLegacy(null).views).toEqual([]);
    expect(parseLegacy('{').views).toEqual([]);
    expect(parseLegacy('{"views":3}').prefs.selected).toBe('all');
  });
});

describe('import into own views', () => {
  it('appends, keeps ids, and skips what is already there', () => {
    const mine = [view('m1', 'Yard')];
    const merged = mergeImported(mine, [view('m1', 'Dock', ['b']), view('x', 'Yard'), view('ok', 'Gate')], makeId);
    expect(merged.map((v) => v.name)).toEqual(['Yard', 'Dock', 'Gate']);
    expect(merged[1]?.id).toMatch(/^gen-/); // m1 was taken
    expect(merged[2]?.id).toBe('ok');
  });
  it('replaces ids the server would refuse', () => {
    expect(mergeImported([], [view('has space', 'A')], makeId)[0]?.id).toMatch(/^gen-/);
  });
  it('stops at 50 views', () => {
    const many = Array.from({ length: 49 }, (_, i) => view(`v${i}`, `V${i}`));
    expect(mergeImported(many, [view('a', 'A'), view('b', 'B')], makeId)).toHaveLength(50);
  });
});

describe('helpers', () => {
  it('upsert replaces by id or appends', () => {
    const list = [view('a', 'A'), view('b', 'B')];
    expect(upsertView(list, view('a', 'A2')).map((v) => v.name)).toEqual(['A2', 'B']);
    expect(upsertView(list, view('c', 'C'))).toHaveLength(3);
  });
  it('a shared copy gets a free id', () => {
    expect(sharedCopy(view('a', 'A'), [], makeId).id).toBe('a');
    expect(sharedCopy(view('a', 'A'), [view('a', 'Other')], makeId).id).toMatch(/^gen-/);
  });
  it('free ids and keys', () => {
    expect(freeViewId('all', [], makeId)).toMatch(/^gen-/);
    expect(viewKey('all', 'x')).toBe('all');
    expect(viewKey('shared', 'x')).toBe('shared:x');
  });
});

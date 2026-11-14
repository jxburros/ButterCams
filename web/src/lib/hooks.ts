import { useCallback, useEffect, useRef, useState } from 'react';
import { errorMessage } from '../api/client';

export const REFRESH_MS = 5000;

export interface Loaded<T> {
  /** The latest successful result, or `null` before the first one. */
  data: T | null;
  /** Why the latest attempt failed, or `null` when it succeeded. */
  error: string | null;
  /** `true` until the first attempt has finished. */
  loading: boolean;
  /** Loads again now (for example after saving a change). */
  reload: () => Promise<void>;
  /** Replaces the data with a value the caller already has (a PUT response). */
  replace: (data: T) => void;
}

/**
 * Loads data when the component mounts and, when `intervalMs` is given, again
 * on that interval (plain polling). A poll is skipped while an earlier request
 * is still running, and an older reply never overwrites a newer one.
 */
export function useLoad<T>(loader: () => Promise<T>, intervalMs: number | null = null): Loaded<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const loaderRef = useRef(loader);
  const aliveRef = useRef(true);
  const sequenceRef = useRef(0);
  const inFlightRef = useRef(0);

  useEffect(() => {
    loaderRef.current = loader;
  });

  const run = useCallback(async (fromTimer: boolean): Promise<void> => {
    if (fromTimer && inFlightRef.current > 0) return;
    sequenceRef.current += 1;
    const sequence = sequenceRef.current;
    inFlightRef.current += 1;
    try {
      const result = await loaderRef.current();
      if (aliveRef.current && sequence === sequenceRef.current) {
        setData(result);
        setError(null);
      }
    } catch (err) {
      if (aliveRef.current && sequence === sequenceRef.current) {
        setError(errorMessage(err));
      }
    } finally {
      inFlightRef.current -= 1;
      if (aliveRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    aliveRef.current = true;
    void run(false);
    const timer = intervalMs === null ? null : window.setInterval(() => void run(true), intervalMs);
    return () => {
      aliveRef.current = false;
      if (timer !== null) window.clearInterval(timer);
    };
  }, [run, intervalMs]);

  const reload = useCallback(() => run(false), [run]);

  const replace = useCallback((value: T) => {
    sequenceRef.current += 1; // a reply already on its way is now out of date
    setData(value);
    setError(null);
  }, []);

  return { data, error, loading, reload, replace };
}

/** Sets the browser tab title for a page. */
export function usePageTitle(title: string): void {
  useEffect(() => {
    document.title = `${title} – ButterCams`;
  }, [title]);
}

/** The current time, refreshed every second, for "… ago" columns. */
export function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}

// Display formatting helpers. Everything returns plain text.

const NONE = '–';

const BYTE_UNITS = ['bytes', 'KB', 'MB', 'GB', 'TB', 'PB'] as const;

export const BYTES_PER_GB = 1024 ** 3;

/**
 * File and disk sizes the way Windows shows them (1 GB = 1024 MB), so the
 * numbers match what the operator sees in Explorer.
 */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes)) return NONE;
  let value = Math.max(0, bytes);
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  if (unit === 0) return Math.round(value) === 1 ? '1 byte' : `${Math.round(value)} bytes`;
  const digits = value >= 100 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${BYTE_UNITS[unit]}`;
}

/** A size given in whole GB (config values such as capGB). */
export function formatGB(gb: number | null | undefined): string {
  if (gb === null || gb === undefined || !Number.isFinite(gb)) return NONE;
  return formatBytes(gb * BYTES_PER_GB);
}

export function formatBitrate(kbps: number | null | undefined): string {
  if (kbps === null || kbps === undefined || !Number.isFinite(kbps) || kbps <= 0) return NONE;
  if (kbps < 1000) return `${Math.round(kbps)} kbps`;
  const mbps = kbps / 1000;
  return `${mbps.toFixed(mbps >= 100 ? 0 : 1)} Mbps`;
}

/** A length of time, two units at most: "45 s", "3 min 20 s", "2 h 5 min", "12 d 4 h". */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return NONE;
  const total = Math.max(0, Math.floor(seconds));
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const secs = total % 60;
  if (days > 0) return hours > 0 ? `${days} d ${hours} h` : `${days} d`;
  if (hours > 0) return minutes > 0 ? `${hours} h ${minutes} min` : `${hours} h`;
  if (minutes > 0) return secs > 0 ? `${minutes} min ${secs} s` : `${minutes} min`;
  return `${secs} s`;
}

function parseTime(ts: string | null | undefined): number | null {
  if (!ts) return null;
  const ms = Date.parse(ts);
  return Number.isNaN(ms) ? null : ms;
}

/** How long ago a timestamp was, measured against this PC's clock: "4 s ago". */
export function formatAge(ts: string | null | undefined, nowMs: number = Date.now()): string {
  const ms = parseTime(ts);
  if (ms === null) return NONE;
  const seconds = Math.max(0, (nowMs - ms) / 1000);
  if (seconds < 1) return 'just now';
  return `${formatDuration(seconds)} ago`;
}

const dateTimeFormat = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
});

const timeFormat = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
});

const dateFormat = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
});

/** Date and time in the browser's local time zone. */
export function formatDateTime(ts: string | null | undefined): string {
  const ms = parseTime(ts);
  return ms === null ? NONE : dateTimeFormat.format(ms);
}

/** Time of day in the browser's local time zone. */
export function formatTime(ts: string | null | undefined): string {
  const ms = parseTime(ts);
  return ms === null ? NONE : timeFormat.format(ms);
}

/** Calendar date in the browser's local time zone. */
export function formatDate(ts: string | null | undefined): string {
  const ms = parseTime(ts);
  return ms === null ? NONE : dateFormat.format(ms);
}

export function formatResolution(width: number | null | undefined, height: number | null | undefined): string {
  if (!width || !height) return NONE;
  return `${width}×${height}`;
}

export function formatFps(fps: number | null | undefined): string {
  if (!fps || !Number.isFinite(fps)) return NONE;
  return `${Number(fps.toFixed(2))} fps`;
}

/** Keyframe interval in seconds: "1 s", "2.5 s". */
export function formatKeyframeInterval(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms)) return NONE;
  return `${Number((ms / 1000).toFixed(2))} s`;
}

/** Codec names as people know them: "h264" → "H.264", "pcm_mulaw" → "G.711 µ-law". */
export function formatCodec(codec: string | null | undefined): string {
  if (!codec) return NONE;
  const known: Record<string, string> = {
    h264: 'H.264',
    h265: 'H.265',
    hevc: 'H.265',
    aac: 'AAC',
    pcm_mulaw: 'G.711 µ-law',
    pcm_alaw: 'G.711 A-law',
    g726: 'G.726',
    opus: 'Opus',
  };
  return known[codec.toLowerCase()] ?? codec;
}

/** "hour_rollover" → "Hour rollover". */
export function humanize(code: string): string {
  const text = code.replace(/[_-]+/g, ' ').trim();
  return text === '' ? NONE : text.charAt(0).toUpperCase() + text.slice(1);
}

/** Today's date as YYYY-MM-DD in the browser's local time zone. */
export function todayLocal(): string {
  const now = new Date();
  const month = String(now.getMonth() + 1).padStart(2, '0');
  const day = String(now.getDate()).padStart(2, '0');
  return `${now.getFullYear()}-${month}-${day}`;
}

export const EMPTY = NONE;

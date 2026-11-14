// Splits a pasted rtsp:// URL into the camera form's fields (spec 5.2).
//
// A hand-written parser is used instead of the browser's URL class because
// camera passwords are often pasted without percent-encoding.

export interface RtspUrlParts {
  host: string;
  port: number;
  path: string;
  /** `null` when the URL carries no credentials. */
  username: string | null;
  password: string | null;
}

export type RtspUrlResult = { ok: true; parts: RtspUrlParts } | { ok: false; message: string };

export const DEFAULT_RTSP_PORT = 554;

function decode(text: string): string {
  try {
    return decodeURIComponent(text);
  } catch {
    return text;
  }
}

function fail(message: string): RtspUrlResult {
  return { ok: false, message };
}

export function parseRtspUrl(input: string): RtspUrlResult {
  const url = input.trim();
  if (url === '') return fail('Paste the full RTSP URL of the camera.');
  if (/\s/.test(url)) return fail('The URL must not contain spaces.');

  const scheme = /^([A-Za-z][A-Za-z0-9+.-]*):\/\//.exec(url);
  if (!scheme || (scheme[1] ?? '').toLowerCase() !== 'rtsp') {
    return fail('The URL must start with rtsp://');
  }
  const rest = url.slice(scheme[0].length);

  // The authority (credentials, host, port) ends at the first "/" or "?".
  const end = rest.search(/[/?]/);
  const authority = end === -1 ? rest : rest.slice(0, end);
  let path = end === -1 ? '' : rest.slice(end);
  if (path.startsWith('?')) path = `/${path}`;

  let username: string | null = null;
  let password: string | null = null;
  let hostPort = authority;
  const at = authority.lastIndexOf('@');
  if (at !== -1) {
    const userInfo = authority.slice(0, at);
    hostPort = authority.slice(at + 1);
    const colon = userInfo.indexOf(':');
    username = decode(colon === -1 ? userInfo : userInfo.slice(0, colon));
    password = colon === -1 ? '' : decode(userInfo.slice(colon + 1));
  }

  let host = hostPort;
  let portText = '';
  if (hostPort.startsWith('[')) {
    // IPv6 literal: [fd00::50]:554
    const close = hostPort.indexOf(']');
    if (close === -1) return fail('The IPv6 address in the URL is missing its closing bracket.');
    host = hostPort.slice(0, close + 1);
    const after = hostPort.slice(close + 1);
    if (after !== '' && !after.startsWith(':')) return fail('The URL has unexpected text after the IPv6 address.');
    portText = after.slice(1);
  } else {
    const colon = hostPort.lastIndexOf(':');
    if (colon !== -1) {
      host = hostPort.slice(0, colon);
      portText = hostPort.slice(colon + 1);
    }
  }

  if (host === '' || host === '[]') return fail('The URL has no host. It should look like rtsp://192.168.1.50:554/stream1');

  let port = DEFAULT_RTSP_PORT;
  if (hostPort.endsWith(':') || portText !== '') {
    if (!/^\d{1,5}$/.test(portText) || Number(portText) < 1 || Number(portText) > 65535) {
      return fail('The port in the URL must be a number between 1 and 65535.');
    }
    port = Number(portText);
  }

  return { ok: true, parts: { host, port, path, username, password } };
}

// Client-side validation helpers. The server validates everything again; these
// exist so the operator gets a specific message before a request is sent.

/** A whole, non-negative number written with digits only, or `null`. */
export function parseWholeNumber(text: string): number | null {
  const trimmed = text.trim();
  if (!/^\d{1,15}$/.test(trimmed)) return null;
  return Number(trimmed);
}

/** Returns a message when `text` is not a whole number in [min, max]. */
export function checkWholeNumber(text: string, min: number, max: number, message: string): string | null {
  const value = parseWholeNumber(text);
  return value === null || value < min || value > max ? message : null;
}

export function checkPort(text: string): string | null {
  return checkWholeNumber(text, 1, 65535, 'Port must be a number between 1 and 65535.');
}

export function isIPv4(text: string): boolean {
  const parts = text.split('.');
  if (parts.length !== 4) return false;
  return parts.every((part) => /^\d{1,3}$/.test(part) && Number(part) <= 255);
}

export function isIPv6(text: string): boolean {
  if (!/^[0-9A-Fa-f:.]+$/.test(text) || !text.includes(':')) return false;
  const halves = text.split('::');
  if (halves.length > 2) return false;

  const groupsOf = (half: string): string[] => (half === '' ? [] : half.split(':'));
  const head = groupsOf(halves[0] ?? '');
  const tail = halves.length === 2 ? groupsOf(halves[1] ?? '') : [];
  const groups = [...head, ...tail];

  let count = 0;
  for (let i = 0; i < groups.length; i += 1) {
    const group = groups[i] ?? '';
    if (i === groups.length - 1 && group.includes('.')) {
      // An IPv4 address in the last position takes the place of two groups.
      if (!isIPv4(group)) return false;
      count += 2;
    } else if (/^[0-9A-Fa-f]{1,4}$/.test(group)) {
      count += 1;
    } else {
      return false;
    }
  }
  return halves.length === 2 ? count <= 7 : count === 8;
}

export function isIPAddress(text: string): boolean {
  return isIPv4(text) || isIPv6(text);
}

/** CIDR notation such as 192.168.1.0/24 or fd00::/8. */
export function isCidr(text: string): boolean {
  const slash = text.indexOf('/');
  if (slash <= 0) return false;
  const address = text.slice(0, slash);
  const prefix = parseWholeNumber(text.slice(slash + 1));
  if (prefix === null) return false;
  if (isIPv4(address)) return prefix <= 32;
  if (isIPv6(address)) return prefix <= 128;
  return false;
}

/** Returns a message when `host` cannot be the host part of an RTSP URL. */
export function checkCameraHost(host: string): string | null {
  const value = host.trim();
  if (value === '') return "Enter the camera's IP address or host name.";
  if (/^\d+(\.\d+){3}$/.test(value)) {
    return isIPv4(value)
      ? null
      : 'That is not a valid IP address. Each of the four numbers must be between 0 and 255.';
  }
  if (value.startsWith('[') && value.endsWith(']')) {
    return isIPv6(value.slice(1, -1)) ? null : 'That is not a valid IPv6 address.';
  }
  if (isIPv6(value)) {
    return 'Put square brackets around an IPv6 address, for example [fd00::50].';
  }
  if (/^[A-Za-z0-9_]([A-Za-z0-9_.-]*[A-Za-z0-9_])?$/.test(value)) return null;
  return 'Host must be only an IP address or host name, without rtsp://, a port or a path. To use a full URL, paste it into the RTSP URL box.';
}

const ID_PATTERN = /^[a-z0-9-]{1,48}$/;

export function checkId(id: string): string | null {
  return ID_PATTERN.test(id)
    ? null
    : 'ID must be 1 to 48 characters, using only lowercase letters, digits and hyphens.';
}

/** Builds an id from a display name: "Front Door #2" → "front-door-2". */
export function idFromName(name: string): string {
  return name
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+/, '')
    .slice(0, 48)
    .replace(/-+$/, '');
}

/**
 * A full local path: a drive letter path such as E:\Recordings, or an absolute
 * path starting with "/" (a development server on Linux or macOS). The server
 * knows which of the two it accepts and has the final word.
 */
export function looksLikeDrivePath(path: string): boolean {
  return /^[A-Za-z]:[\\/]/.test(path) || path.startsWith('/');
}

/** A share path such as \\server\share or \\server\share\folder. */
export function looksLikeUncPath(path: string): boolean {
  return /^\\\\[^\\/]+\\[^\\/]+/.test(path);
}

export function checkPassword(password: string): string | null {
  return password.length >= 8 ? null : 'Password must be at least 8 characters.';
}

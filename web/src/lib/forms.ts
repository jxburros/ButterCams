// Shared form plumbing: a field → message map, and turning an API error into one.

import { ApiError, errorMessage } from '../api/client';

/** Message per form field. A field without a message is valid. */
export type FieldErrors = Record<string, string>;

export function hasErrors(errors: FieldErrors): boolean {
  return Object.keys(errors).length > 0;
}

export interface ServerErrors {
  /** Messages for inputs on this form, keyed by the form's field names. */
  fields: FieldErrors;
  /** What to show at the top of the form. */
  message: string;
}

/**
 * Maps the `fields` of a `validation_failed` error onto a form's inputs.
 *
 * `known` lists the form's field names. A server field matches by its full
 * name or by its last segment, so "server.port" and "allowedSubnets[1]" land
 * on the "port" and "allowedSubnets" inputs. Anything that matches no input is
 * added to the top-of-form message so that no server message is lost.
 */
export function serverErrors(err: unknown, known: readonly string[]): ServerErrors {
  const message = errorMessage(err);
  if (!(err instanceof ApiError) || err.fields.length === 0) {
    return { fields: {}, message };
  }

  const fields: FieldErrors = {};
  const unmatched: string[] = [];
  for (const item of err.fields) {
    const key = matchField(item.field, known);
    if (key === null) {
      unmatched.push(item.field === '' ? item.message : `${item.field}: ${item.message}`);
    } else if (fields[key] === undefined) {
      fields[key] = item.message;
    } else {
      fields[key] = `${fields[key]} ${item.message}`;
    }
  }
  return { fields, message: [message, ...unmatched].join(' ') };
}

function matchField(field: string, known: readonly string[]): string | null {
  if (known.includes(field)) return field;
  const withoutIndex = field.replace(/\[\d+\]/g, '');
  if (known.includes(withoutIndex)) return withoutIndex;
  const last = withoutIndex.split('.').pop() ?? '';
  return known.includes(last) ? last : null;
}

/** Reads a small text file chosen in a file input. */
export async function readTextFile(file: File, maxBytes: number): Promise<string> {
  if (file.size > maxBytes) {
    throw new Error(`"${file.name}" is too large to be the right file.`);
  }
  return file.text();
}

/** Base64 of a file's bytes (for the PFX upload). */
export async function readFileBase64(file: File, maxBytes: number): Promise<string> {
  if (file.size > maxBytes) {
    throw new Error(`"${file.name}" is too large to be the right file.`);
  }
  const bytes = new Uint8Array(await file.arrayBuffer());
  let binary = '';
  const chunk = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += chunk) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunk));
  }
  return btoa(binary);
}

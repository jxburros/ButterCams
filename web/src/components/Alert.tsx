import type { ReactNode } from 'react';

type AlertKind = 'error' | 'success' | 'warning' | 'info';

/**
 * A message box. Errors are announced immediately to screen readers; the
 * other kinds are announced politely.
 */
export function Alert({ kind, children }: { kind: AlertKind; children: ReactNode }) {
  return (
    <div className={`alert alert-${kind}`} role={kind === 'error' ? 'alert' : 'status'}>
      {children}
    </div>
  );
}

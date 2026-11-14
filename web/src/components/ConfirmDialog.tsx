import { useEffect, useId, useRef } from 'react';
import type { ReactNode } from 'react';
import { Alert } from './Alert';

interface ConfirmDialogProps {
  title: string;
  children: ReactNode;
  confirmLabel: string;
  /** Styles the confirm button as destructive. */
  danger?: boolean;
  /** `true` while the confirmed action is running. */
  busy?: boolean;
  /** Why the action failed; the dialog stays open to show it. */
  error?: string | null;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * A modal confirmation built on the native <dialog>, which traps focus and
 * closes on Escape. Mount it to open it; unmount it to close it.
 */
export function ConfirmDialog({
  title,
  children,
  confirmLabel,
  danger = false,
  busy = false,
  error = null,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const titleId = useId();

  useEffect(() => {
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
    // Start on Cancel so that a stray Enter never confirms.
    cancelRef.current?.focus();
    return () => {
      if (dialog?.open) dialog.close();
    };
  }, []);

  return (
    <dialog
      ref={ref}
      className="dialog"
      aria-labelledby={titleId}
      onCancel={(event) => {
        event.preventDefault();
        if (!busy) onCancel();
      }}
    >
      <h2 id={titleId}>{title}</h2>
      <div className="dialog-body">{children}</div>
      {error ? <Alert kind="error">{error}</Alert> : null}
      <div className="button-row">
        <button type="button" className={danger ? 'button button-danger' : 'button button-primary'} disabled={busy} onClick={onConfirm}>
          {busy ? 'Working…' : confirmLabel}
        </button>
        <button ref={cancelRef} type="button" className="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </dialog>
  );
}

// Labelled form inputs. Each one wires its label, hint and error message to
// the input for screen readers.

import { useId } from 'react';
import type { InputHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from 'react';

interface FieldBase {
  label: string;
  /** Validation message; shown next to the input when set. */
  error?: string;
  hint?: ReactNode;
}

function describedBy(id: string, hint: ReactNode, error: string | undefined): string | undefined {
  const ids = [hint ? `${id}-hint` : '', error ? `${id}-error` : ''].filter((part) => part !== '');
  return ids.length > 0 ? ids.join(' ') : undefined;
}

function Messages({ id, hint, error }: { id: string; hint: ReactNode; error: string | undefined }) {
  return (
    <>
      {hint ? (
        <p id={`${id}-hint`} className="field-hint">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={`${id}-error`} className="field-error">
          {error}
        </p>
      ) : null}
    </>
  );
}

type TextFieldProps = FieldBase &
  Omit<InputHTMLAttributes<HTMLInputElement>, 'id' | 'value' | 'onChange'> & {
    value: string;
    onChange: (value: string) => void;
  };

export function TextField({ label, error, hint, value, onChange, type = 'text', ...rest }: TextFieldProps) {
  const id = useId();
  return (
    <div className={error ? 'field field-invalid' : 'field'}>
      <label htmlFor={id}>{label}</label>
      <input
        {...rest}
        id={id}
        type={type}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, hint, error)}
      />
      <Messages id={id} hint={hint} error={error} />
    </div>
  );
}

type TextAreaFieldProps = FieldBase &
  Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, 'id' | 'value' | 'onChange'> & {
    value: string;
    onChange: (value: string) => void;
  };

export function TextAreaField({ label, error, hint, value, onChange, ...rest }: TextAreaFieldProps) {
  const id = useId();
  return (
    <div className={error ? 'field field-invalid' : 'field'}>
      <label htmlFor={id}>{label}</label>
      <textarea
        {...rest}
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, hint, error)}
      />
      <Messages id={id} hint={hint} error={error} />
    </div>
  );
}

type SelectFieldProps = FieldBase &
  Omit<SelectHTMLAttributes<HTMLSelectElement>, 'id' | 'value' | 'onChange'> & {
    value: string;
    onChange: (value: string) => void;
    children: ReactNode;
  };

export function SelectField({ label, error, hint, value, onChange, children, ...rest }: SelectFieldProps) {
  const id = useId();
  return (
    <div className={error ? 'field field-invalid' : 'field'}>
      <label htmlFor={id}>{label}</label>
      <select
        {...rest}
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, hint, error)}
      >
        {children}
      </select>
      <Messages id={id} hint={hint} error={error} />
    </div>
  );
}

type CheckboxFieldProps = FieldBase &
  Omit<InputHTMLAttributes<HTMLInputElement>, 'id' | 'type' | 'checked' | 'onChange'> & {
    checked: boolean;
    onChange: (checked: boolean) => void;
  };

export function CheckboxField({ label, error, hint, checked, onChange, ...rest }: CheckboxFieldProps) {
  const id = useId();
  return (
    <div className={error ? 'field field-check field-invalid' : 'field field-check'}>
      <div className="check-row">
        <input
          {...rest}
          id={id}
          type="checkbox"
          checked={checked}
          onChange={(event) => onChange(event.target.checked)}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy(id, hint, error)}
        />
        <label htmlFor={id}>{label}</label>
      </div>
      <Messages id={id} hint={hint} error={error} />
    </div>
  );
}

interface FilePickerProps extends FieldBase {
  /** File name filter for the picker, for example ".csv,text/csv". */
  accept?: string;
  disabled?: boolean;
  /** Called with the chosen file, or `null` when the choice is cleared. */
  onPick: (file: File | null) => void;
}

export function FilePicker({ label, error, hint, accept, disabled, onPick }: FilePickerProps) {
  const id = useId();
  return (
    <div className={error ? 'field field-invalid' : 'field'}>
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        type="file"
        accept={accept}
        disabled={disabled}
        onChange={(event) => onPick(event.target.files?.[0] ?? null)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(id, hint, error)}
      />
      <Messages id={id} hint={hint} error={error} />
    </div>
  );
}

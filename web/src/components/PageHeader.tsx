import type { ReactNode } from 'react';

/** The top of every page: a small label, the big title and the page's main actions. */
export function PageHeader({ eyebrow, title, children }: { eyebrow?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="page-header">
      <div className="page-title">
        {eyebrow ? <p className="eyebrow">{eyebrow}</p> : null}
        <h1>{title}</h1>
      </div>
      {children ? <div className="page-actions">{children}</div> : null}
    </div>
  );
}

/** A row of buttons where exactly one is chosen (layouts, filters). */
export function Segmented<T extends string>({
  label,
  options,
  value,
  onChange,
}: {
  label: string;
  options: { value: T; label: ReactNode; ariaLabel?: string }[];
  value: T;
  onChange: (value: T) => void;
}) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          className={option.value === value ? 'seg-option seg-option-on' : 'seg-option'}
          aria-pressed={option.value === value}
          aria-label={option.ariaLabel}
          onClick={() => onChange(option.value)}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

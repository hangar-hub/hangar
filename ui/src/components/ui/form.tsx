import { Switch as RadixSwitch } from "radix-ui";
import {
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
  useId,
} from "react";
import { cn } from "@/lib/utils";

const control =
  "w-full rounded-md border border-line-strong bg-surface px-3 text-sm text-ink transition-[border-color] duration-150 hover:border-steel-400 placeholder:text-muted/80 focus-visible:outline-2 focus-visible:outline-offset-0 focus-visible:outline-accent disabled:opacity-60 aria-[invalid=true]:border-danger";

/** Compact controls sit in toolbars beside `sm` buttons. */
type Compact = { compact?: boolean };

export function Input({
  className,
  compact,
  ...props
}: InputHTMLAttributes<HTMLInputElement> & Compact) {
  return <input className={cn(control, compact ? "h-7 text-xs" : "h-9", className)} {...props} />;
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return (
    <textarea className={cn(control, "min-h-24 py-2 leading-relaxed", className)} {...props} />
  );
}

export function Select({
  className,
  compact,
  ...props
}: SelectHTMLAttributes<HTMLSelectElement> & Compact) {
  return (
    <select
      className={cn(control, compact ? "h-7 w-auto pr-7 pl-2 text-xs" : "h-9 pr-8", className)}
      {...props}
    />
  );
}

/**
 * A labelled form field. The child receives the generated ID through the
 * render function, so the label, hint and error are all linked.
 */
export function Field({
  label,
  hint,
  error,
  children,
  className,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: string;
  className?: string;
  children: (props: {
    id: string;
    "aria-describedby"?: string;
    "aria-invalid"?: boolean;
  }) => ReactNode;
}) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy =
    [hint ? hintId : "", error ? errorId : ""].filter(Boolean).join(" ") || undefined;
  return (
    <div className={cn("space-y-1.5", className)}>
      <label htmlFor={id} className="block text-sm font-medium">
        {label}
      </label>
      {children({ id, "aria-describedby": describedBy, "aria-invalid": error ? true : undefined })}
      {hint ? (
        <p id={hintId} className="text-xs text-muted">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} className="text-xs text-danger">
          {error}
        </p>
      ) : null}
    </div>
  );
}

/**
 * A checkbox with its label. `card` frames it in a bordered row that is
 * all one click target and highlights when checked, for lists of options.
 */
export function Checkbox({
  label,
  hint,
  card,
  className,
  ...props
}: InputHTMLAttributes<HTMLInputElement> & {
  label: ReactNode;
  hint?: ReactNode;
  card?: boolean;
}) {
  const id = useId();
  if (card) {
    return (
      <label
        htmlFor={id}
        className={cn(
          "flex cursor-pointer items-start gap-2 rounded-md border border-line px-3 py-2 transition-[border-color,background-color] duration-150 hover:border-line-strong has-checked:border-accent/50 has-checked:bg-accent-soft/50",
          className,
        )}
      >
        <input
          id={id}
          type="checkbox"
          className="mt-0.5 size-4 shrink-0 accent-[var(--hg-accent-fill)]"
          aria-describedby={hint ? `${id}-hint` : undefined}
          {...props}
        />
        <span className="text-sm leading-tight">
          {label}
          {hint ? (
            <span id={`${id}-hint`} className="mt-0.5 block text-xs text-muted">
              {hint}
            </span>
          ) : null}
        </span>
      </label>
    );
  }
  return (
    <div className={cn("flex items-start gap-2", className)}>
      <input
        id={id}
        type="checkbox"
        className="mt-0.5 size-4 shrink-0 accent-[var(--hg-accent-fill)]"
        aria-describedby={hint ? `${id}-hint` : undefined}
        {...props}
      />
      <div className="text-sm leading-tight">
        <label htmlFor={id}>{label}</label>
        {hint ? (
          <p id={`${id}-hint`} className="mt-0.5 text-xs text-muted">
            {hint}
          </p>
        ) : null}
      </div>
    </div>
  );
}

/** An on and off switch with its label, for settings. */
export function Switch({
  label,
  hint,
  checked,
  onCheckedChange,
  disabled,
  className,
}: {
  label: ReactNode;
  hint?: ReactNode;
  checked: boolean;
  onCheckedChange: (v: boolean) => void;
  disabled?: boolean;
  /** Replaces the default vertical padding, for rows with their own rhythm. */
  className?: string;
}) {
  const id = useId();
  return (
    <div className={cn("flex items-start justify-between gap-4 py-2", className)}>
      <div className="text-sm">
        <label htmlFor={id} className="font-medium">
          {label}
        </label>
        {hint ? (
          <p id={`${id}-hint`} className="mt-0.5 text-xs text-muted">
            {hint}
          </p>
        ) : null}
      </div>
      <RadixSwitch.Root
        id={id}
        checked={checked}
        disabled={disabled}
        onCheckedChange={onCheckedChange}
        aria-describedby={hint ? `${id}-hint` : undefined}
        className="relative mt-0.5 inline-flex h-5 w-9 shrink-0 items-center rounded-full border border-line-strong bg-raised transition-[background-color,border-color] duration-150 ease-out data-[state=checked]:border-accent data-[state=checked]:bg-accent-fill disabled:opacity-50"
      >
        <RadixSwitch.Thumb className="block size-4 translate-x-0.5 rounded-full bg-surface shadow transition-[translate] duration-200 ease-out data-[state=checked]:translate-x-[18px] data-[state=checked]:bg-accent-ink" />
      </RadixSwitch.Root>
    </div>
  );
}

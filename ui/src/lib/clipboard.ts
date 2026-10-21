/**
 * Copies text to the clipboard. The Clipboard API only exists in secure
 * contexts, and many Instances are reached over plain HTTP on a private
 * network, so when it is missing or refuses, this falls back to selecting
 * the text in a temporary textarea and running the copy command.
 *
 * `container` is where the textarea goes: inside an open dialog, so the
 * dialog's focus trap does not pull focus back before the copy runs.
 * Returns whether the text was copied.
 */
export async function copyText(text: string, container: Element = document.body): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Refused, for example without a secure context: try the fallback.
    }
  }
  return copyWithTextarea(text, container);
}

function copyWithTextarea(text: string, container: Element): boolean {
  if (typeof document.execCommand !== "function") return false;
  const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.setAttribute("aria-hidden", "true");
  area.tabIndex = -1;
  // Off screen but still selectable. CSSOM styles are allowed by the
  // Content Security Policy, unlike style attributes.
  area.style.position = "fixed";
  area.style.top = "0";
  area.style.left = "-9999px";
  area.style.opacity = "0";
  container.appendChild(area);
  let copied = false;
  try {
    area.focus({ preventScroll: true });
    area.select();
    area.setSelectionRange(0, text.length);
    copied = document.execCommand("copy");
  } catch {
    copied = false;
  } finally {
    area.remove();
    previous?.focus({ preventScroll: true });
  }
  return copied;
}

/** Selects an element's text, so the user can copy it with the keyboard. */
export function selectText(el: Element): void {
  const selection = window.getSelection();
  if (!selection) return;
  selection.removeAllRanges();
  const range = document.createRange();
  range.selectNodeContents(el);
  selection.addRange(range);
}

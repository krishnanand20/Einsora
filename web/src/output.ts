import type { OutputChunk, OutputPage } from "./api/types";

/**
 * A workload's output as the dashboard shows it (ADR-0024): the most recent output, up to
 * MAX_SHOWN_CHARS, and where to read on from. What isn't shown here is in Download full log.
 */
export interface OutputView {
  chunks: OutputChunk[];
  next: number;
  collected: boolean;
  notes: string[];
  /** Characters of earlier output left out of this page, to keep it light. */
  hiddenChars: number;
}

export const MAX_SHOWN_CHARS = 1 << 20;

/** Adds a page of output to what's shown, keeping the most recent MAX_SHOWN_CHARS. */
export function mergeOutput(prev: OutputView | undefined, page: OutputPage, max = MAX_SHOWN_CHARS): OutputView {
  const seen = new Set(prev?.chunks.map((c) => c.id));
  const chunks = [...(prev?.chunks ?? []), ...page.items.filter((c) => !seen.has(c.id))];
  let shown = chunks.reduce((n, c) => n + c.text.length, 0);
  let hiddenChars = prev?.hiddenChars ?? 0;
  while (shown > max && chunks.length > 1) {
    const dropped = chunks.shift();
    shown -= dropped?.text.length ?? 0;
    hiddenChars += dropped?.text.length ?? 0;
  }
  return { chunks, next: Math.max(page.next, prev?.next ?? 0), collected: page.collected, notes: page.notes, hiddenChars };
}

// The escape sequences programs print for colours, cursor movement, and window titles.
const ESCAPES = /\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])/g;
const CONTROLS = /[\x00-\x08\x0b-\x1f\x7f-\x9f]/g;

/**
 * Output as text to show: escape sequences removed, carriage returns (a progress bar redrawing its
 * line) as newlines, and other control characters dropped. Newlines and tabs stay. React escapes
 * the rest when it renders it.
 */
export function outputText(text: string): string {
  return text.replace(ESCAPES, "").replace(/\r\n?/g, "\n").replace(CONTROLS, "");
}

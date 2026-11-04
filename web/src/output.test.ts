import { describe, expect, it } from "vitest";

import type { OutputChunk } from "./api/types";
import { mergeOutput, outputText } from "./output";

const chunk = (id: number, text: string, stream: OutputChunk["stream"] = "stdout"): OutputChunk => ({
  id,
  stream,
  text,
  at: "2026-11-04T10:00:00Z",
});

describe("mergeOutput", () => {
  it("adds new chunks once, and keeps the reader's place", () => {
    const first = mergeOutput(undefined, { items: [chunk(1, "a\n"), chunk(2, "b\n")], next: 2, collected: true, notes: [] });
    const again = mergeOutput(first, { items: [chunk(2, "b\n"), chunk(3, "c\n")], next: 3, collected: true, notes: ["n"] });
    expect(again.chunks.map((c) => c.id)).toEqual([1, 2, 3]);
    expect(again.next).toBe(3);
    expect(again.notes).toEqual(["n"]);
  });

  it("keeps the most recent output, and counts what it leaves out", () => {
    const view = mergeOutput(undefined, { items: [chunk(1, "aaaa"), chunk(2, "bbbb"), chunk(3, "cc")], next: 3, collected: true, notes: [] }, 6);
    expect(view.chunks.map((c) => c.id)).toEqual([2, 3]);
    expect(view.hiddenChars).toBe(4);
  });

  it("never goes back to an earlier place", () => {
    const view = mergeOutput({ chunks: [], next: 9, collected: true, notes: [], hiddenChars: 0 }, { items: [], next: 4, collected: true, notes: [] });
    expect(view.next).toBe(9);
  });
});

describe("outputText", () => {
  it.each([
    ["plain\ttext\n", "plain\ttext\n"],
    ["\x1b[1;31mred\x1b[0m", "red"],
    ["\x1b]0;title\x07after", "after"],
    ["10%\r50%\r100%\r\n", "10%\n50%\n100%\n"],
    ["bell\x07 C1\x9b", "bell C1"],
  ])("%j shows as %j", (input, want) => {
    expect(outputText(input)).toBe(want);
  });
});

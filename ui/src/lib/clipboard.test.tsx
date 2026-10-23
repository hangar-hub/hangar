import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CopyButton, CopyField } from "@/components/ui/copy-button";
import { ToastProvider } from "@/components/ui/toast";
import { copyText } from "./clipboard";

// jsdom has neither the Clipboard API nor execCommand, like a browser on
// plain HTTP has no Clipboard API. Each test adds what it needs.
function setClipboard(writeText?: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: writeText ? { writeText } : undefined,
  });
}

function setExecCommand(impl?: (command: string) => boolean) {
  Object.defineProperty(document, "execCommand", { configurable: true, value: impl });
}

describe("copyText", () => {
  beforeEach(() => {
    setClipboard(undefined);
    setExecCommand(undefined);
  });
  afterEach(cleanup);

  it("uses the Clipboard API when the page has it", async () => {
    const writeText = vi.fn(async () => {});
    setClipboard(writeText);
    expect(await copyText("hgr_abc")).toBe(true);
    expect(writeText).toHaveBeenCalledWith("hgr_abc");
  });

  it("falls back to a selected textarea without the Clipboard API, and cleans up", async () => {
    let selected = "";
    const exec = vi.fn((command: string) => {
      const el = document.activeElement as HTMLTextAreaElement;
      selected = el.value.slice(el.selectionStart, el.selectionEnd);
      return command === "copy";
    });
    setExecCommand(exec);
    render(<button type="button">Before</button>);
    screen.getByRole("button", { name: "Before" }).focus();
    expect(await copyText("export HF_ENDPOINT=http://hangar.lan")).toBe(true);
    expect(exec).toHaveBeenCalledWith("copy");
    expect(selected).toBe("export HF_ENDPOINT=http://hangar.lan");
    expect(document.querySelector("textarea")).toBeNull();
    expect(screen.getByRole("button", { name: "Before" })).toHaveFocus();
  });

  it("falls back when the Clipboard API refuses", async () => {
    setClipboard(async () => {
      throw new DOMException("Not allowed", "NotAllowedError");
    });
    setExecCommand(() => true);
    expect(await copyText("x")).toBe(true);
  });

  it("reports failure when neither works", async () => {
    setExecCommand(() => false);
    expect(await copyText("x")).toBe(false);
    setExecCommand(undefined);
    expect(await copyText("x")).toBe(false);
  });
});

describe("CopyButton", () => {
  beforeEach(() => {
    setClipboard(undefined);
    setExecCommand(undefined);
  });
  afterEach(cleanup);

  it("confirms a copy made with the fallback", async () => {
    setExecCommand(() => true);
    render(<CopyButton value="ada/m" label="Copy the Repo ID" />);
    await userEvent.click(screen.getByRole("button", { name: "Copy the Repo ID" }));
    expect(await screen.findByText("Copied")).toBeInTheDocument();
  });

  it("tells the user when copying fails", async () => {
    render(
      <ToastProvider>
        <CopyButton value="ada/m" label="Copy the Repo ID" />
      </ToastProvider>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Copy the Repo ID" }));
    expect(
      await screen.findAllByText(
        "Couldn't copy to the clipboard. Select the text and copy it instead.",
      ),
    ).not.toHaveLength(0);
    expect(screen.queryByText("Copied")).toBeNull();
  });

  it("selects a one-time secret when copying fails, so the keyboard can copy it", async () => {
    render(
      <ToastProvider>
        <CopyField value="hgr_secret" label="Copy the Token" testId="secret" />
      </ToastProvider>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Copy the Token" }));
    expect(window.getSelection()?.toString()).toBe("hgr_secret");
    expect(
      (await screen.findAllByText(/The text is selected: press Ctrl\+C/)).length,
    ).toBeGreaterThan(0);
  });
});

import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RiskyDownload } from "./RiskyDownload";

describe("RiskyDownload", () => {
  afterEach(cleanup);

  it("asks first, then downloads with accept_risk=1", async () => {
    const navigate = vi.fn();
    render(
      <RiskyDownload
        href="/alice/m/resolve/main/evil.pkl?download=true"
        path="evil.pkl"
        status="unsafe"
        navigate={navigate}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(screen.getByText(/can run code or touch the system/)).toBeInTheDocument();
    expect(navigate).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Download anyway" }));
    expect(navigate).toHaveBeenCalledWith(
      "/alice/m/resolve/main/evil.pkl?download=true&accept_risk=1",
    );
  });

  it("explains suspicious imports", async () => {
    const navigate = vi.fn();
    render(
      <RiskyDownload
        href="/alice/m/resolve/main/model.joblib"
        path="model.joblib"
        status="suspicious"
        navigate={navigate}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(screen.getByText(/does not know to be safe/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Download anyway" }));
    expect(navigate).toHaveBeenCalledWith("/alice/m/resolve/main/model.joblib?accept_risk=1");
  });

  it("explains a pending scan", async () => {
    const navigate = vi.fn();
    render(
      <RiskyDownload
        href="/alice/m/resolve/main/w.bin"
        path="w.bin"
        status="pending"
        navigate={navigate}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(screen.getByText(/has not checked this file yet/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(navigate).not.toHaveBeenCalled();
  });
});

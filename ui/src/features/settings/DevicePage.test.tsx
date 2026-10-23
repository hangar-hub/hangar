import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DevicePage } from "./DevicePage";

type Call = { url: string; body: Record<string, unknown> };

/** Answers the two device endpoints and records what the page sent. */
function stubApi(): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal("fetch", async (input: Request) => {
    const body = JSON.parse(await input.text()) as Record<string, unknown>;
    calls.push({ url: new URL(input.url).pathname, body });
    if (input.url.endsWith("/me/device/lookup")) {
      return Response.json({
        user_code: body.user_code,
        client_id: "hangar-cli",
        client_ip: "198.51.100.7",
        requested_at: new Date(Date.now() - 60_000).toISOString(),
        expires_at: new Date(Date.now() + 14 * 60_000).toISOString(),
        token_lifetime_days: 30,
      });
    }
    return new Response(null, { status: 204 });
  });
  return calls;
}

function renderAt(path: string) {
  const root = createRootRoute();
  const device = createRoute({
    getParentRoute: () => root,
    path: "/settings/device",
    component: DevicePage,
  });
  const router = createRouter({
    routeTree: root.addChildren([device]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={new QueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("DevicePage", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("does not fill in a code from the link", async () => {
    stubApi();
    renderAt("/settings/device?code=BCDF-GHJK");
    const input = await screen.findByLabelText("Code");
    expect(input).toHaveValue("");
    expect(screen.getByRole("note")).toHaveTextContent("does not fill in a code from a link");
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
  });

  it("shows who asked before approving, with read access by default", async () => {
    const calls = stubApi();
    renderAt("/settings/device");
    await userEvent.type(await screen.findByLabelText("Code"), "bcdfghjk");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByText("hangar-cli")).toBeInTheDocument();
    expect(screen.getByText("198.51.100.7")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("It expires in 30 days");
    expect(calls).toEqual([
      { url: "/api/hangar/v1/me/device/lookup", body: { user_code: "BCDF-GHJK" } },
    ]);

    await userEvent.click(screen.getByRole("button", { name: "Approve with read access" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Approved");
    expect(calls[1]).toEqual({
      url: "/api/hangar/v1/me/device",
      body: { user_code: "BCDF-GHJK", approve: true, scope: "read" },
    });
  });

  it("gives write access only when the user chooses it", async () => {
    const calls = stubApi();
    renderAt("/settings/device");
    await userEvent.type(await screen.findByLabelText("Code"), "BCDF-GHJK");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await userEvent.click(await screen.findByRole("radio", { name: /write/ }));
    await userEvent.click(screen.getByRole("button", { name: "Approve with write access" }));
    await screen.findByRole("status");
    expect(calls[1]?.body).toEqual({ user_code: "BCDF-GHJK", approve: true, scope: "write" });
  });
});

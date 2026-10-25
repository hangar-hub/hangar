import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OllamaKeysPage, signCommand } from "./OllamaKeysPage";

const challenge = "hangar-ollama-key:0192-ab:1792000000:n0nce_-x:m4c-_y:hangar.example.com:8443";
const signature = "-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----";

describe("OllamaKeysPage", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("builds the ssh-keygen command that signs the challenge", () => {
    expect(signCommand(challenge, "hangar-ollama-key")).toBe(
      `printf '%s' '${challenge}' | ssh-keygen -Y sign -f ~/.ollama/id_ed25519 -n hangar-ollama-key`,
    );
  });

  it("sends the challenge and the key's signature with the key", async () => {
    const posted: unknown[] = [];
    vi.stubGlobal("fetch", async (input: Request) => {
      const path = new URL(input.url).pathname;
      if (path.endsWith("/me/ollama-keys/challenge")) {
        return Response.json({
          challenge,
          namespace: "hangar-ollama-key",
          expires_at: new Date(Date.now() + 900_000).toISOString(),
        });
      }
      if (input.method === "POST") {
        posted.push(JSON.parse(await input.text()));
        return Response.json(
          {
            id: crypto.randomUUID(),
            fingerprint: "SHA256:x",
            created_at: new Date().toISOString(),
          },
          { status: 201 },
        );
      }
      return Response.json({ items: [] });
    });
    render(
      <QueryClientProvider client={new QueryClient()}>
        <OllamaKeysPage />
      </QueryClientProvider>,
    );
    expect(
      await screen.findByText(signCommand(challenge, "hangar-ollama-key")),
    ).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Public key"), "ssh-ed25519 AAAA me@laptop");
    const add = screen.getByRole("button", { name: "Add key" });
    expect(add).toBeDisabled();
    await userEvent.click(screen.getByLabelText("Signature"));
    await userEvent.paste(signature);
    await userEvent.click(add);
    await vi.waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ public_key: "ssh-ed25519 AAAA me@laptop", challenge, signature });
  });
});

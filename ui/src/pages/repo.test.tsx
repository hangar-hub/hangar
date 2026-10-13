import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PageShell } from "@/components/PageShell";
import { RepoPageShell, repoLabel } from "./repo";

describe("page shells", () => {
  it("labels Repos like the Hub", () => {
    expect(repoLabel("model", "ns", "m")).toBe("ns/m");
    expect(repoLabel("dataset", "ns", "d")).toBe("datasets/ns/d");
  });

  it("renders a heading and subtitle", () => {
    render(
      <RepoPageShell kind="dataset" ns="ns" name="d" page="Viewer" detail="default / train" />,
    );
    expect(screen.getByRole("heading", { level: 1, name: "Viewer" })).toBeInTheDocument();
    expect(screen.getByText("datasets/ns/d · default / train")).toBeInTheDocument();
  });

  it("omits the subtitle when there is none", () => {
    render(<PageShell title="Arsenal" />);
    expect(screen.getByRole("heading", { name: "Arsenal" })).toBeInTheDocument();
  });
});

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { isNumericFeature } from "@/api/viewer";
import { Cell } from "./Cell";
import { StatsSummary } from "./ColumnStats";

describe("Viewer cells", () => {
  it("renders class labels with name and id", () => {
    render(
      <Cell
        value={1}
        type={{ _type: "ClassLabel", names: ["neg", "pos"] }}
        truncated={false}
        label="label, row 0"
      />,
    );
    expect(screen.getByText("(1)").parentElement).toHaveTextContent("pos (1)");
  });

  it("expands long text and notes truncation", async () => {
    render(
      <Cell
        value={"word ".repeat(80)}
        type={{ _type: "Value", dtype: "string" }}
        truncated
        label="text, row 0"
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Show more" }));
    expect(screen.getByText(/The Dataset Viewer shortened this cell/)).toBeInTheDocument();
  });

  it("renders images, nulls, numbers and structs", () => {
    const { container } = render(
      <div>
        <Cell
          value={{ src: "/datasets-server/assets/x.png", width: 32, height: 32 }}
          type={{ _type: "Image" }}
          truncated={false}
          label="img, row 3"
        />
        <Cell value={null} type={{ _type: "Value", dtype: "string" }} truncated={false} label="x" />
        <Cell value={12345} type={{ _type: "Value", dtype: "int64" }} truncated={false} label="n" />
        <Cell value={{ a: [1, 2] }} type={{}} truncated={false} label="s" />
      </div>,
    );
    expect(screen.getByRole("img", { name: "img, row 3" })).toHaveAttribute(
      "src",
      "/datasets-server/assets/x.png",
    );
    expect(screen.getByText("null")).toBeInTheDocument();
    expect(screen.getByText("12,345")).toBeInTheDocument();
    expect(container.querySelector("details summary")).toHaveTextContent("1 fields");
  });

  it("shows top values and histograms", () => {
    render(
      <StatsSummary
        stats={{
          column_name: "label",
          column_type: "class_label",
          column_statistics: { n_unique: 2, frequencies: { pos: 30, neg: 10 } },
        }}
      />,
    );
    expect(screen.getByText("2 values")).toBeInTheDocument();
    expect(screen.getByText("30")).toBeInTheDocument();
    expect(isNumericFeature({ _type: "Value", dtype: "float32" })).toBe(true);
    expect(isNumericFeature({ _type: "ClassLabel" })).toBe(true);
    expect(isNumericFeature({ _type: "Value", dtype: "string" })).toBe(false);
  });
});

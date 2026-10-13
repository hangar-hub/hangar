import { describe, expect, it } from "vitest";
import { ApiError, errorMessage, errorStatus, fieldErrors, unwrap, unwrapEmpty } from "./problem";

const res = (status: number) => new Response(null, { status });

describe("problem details", () => {
  it("uses the detail, then field errors, then a message for the status", () => {
    expect(
      new ApiError(400, {
        type: "about:blank",
        title: "Bad Request",
        status: 400,
        detail: "Name is taken.",
      }).message,
    ).toBe("Name is taken.");
    expect(
      new ApiError(400, {
        type: "about:blank",
        title: "Bad Request",
        status: 400,
        errors: [{ detail: "One." }, { detail: "Two." }],
      }).message,
    ).toBe("One. Two.");
    expect(new ApiError(401).message).toBe("Sign in to continue.");
    expect(new ApiError(503).message).toMatch(/Hangar had a problem/);
    expect(new ApiError(418).message).toBe("The request failed with status 418.");
  });

  it("maps field errors by pointer or parameter", () => {
    const err = new ApiError(400, {
      type: "about:blank",
      title: "Bad Request",
      status: 400,
      errors: [
        { pointer: "/name", detail: "Taken." },
        { parameter: "limit", detail: "Too big." },
        { detail: "No key." },
      ],
    });
    expect(fieldErrors(err)).toEqual({ name: "Taken.", limit: "Too big." });
    expect(fieldErrors(new Error("x"))).toEqual({});
  });

  it("unwraps results and throws ApiError with the status", () => {
    expect(unwrap({ data: { ok: true }, response: res(200) })).toEqual({ ok: true });
    expect(() =>
      unwrap({
        error: { type: "t", title: "t", status: 404, detail: "Gone." },
        response: res(404),
      }),
    ).toThrow("Gone.");
    expect(() => unwrapEmpty({ response: res(500) })).toThrow(ApiError);
    expect(() => unwrapEmpty({ response: res(204) })).not.toThrow();
    try {
      unwrap({ error: {}, response: res(403) });
    } catch (e) {
      expect(errorStatus(e)).toBe(403);
      expect(errorMessage(e)).toBe("You don't have permission to do this.");
    }
    expect(errorStatus(new Error("x"))).toBe(0);
    expect(errorMessage("weird")).toBe("Something went wrong. Try again.");
  });
});

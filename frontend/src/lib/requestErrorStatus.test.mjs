import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  buildErrorStatusBreakdown,
  formatErrorStatusPercent,
} from "./requestErrorStatus.ts";

describe("buildErrorStatusBreakdown", () => {
  it("sorts by count then status code and uses the capsule total for percent", () => {
    const rows = buildErrorStatusBreakdown({ 500: 2, 429: 8, 400: 0 }, 10);
    assert.deepEqual(
      rows.map((row) => [row.code, row.count, row.percent]),
      [
        ["429", 8, 80],
        ["500", 2, 20],
      ],
    );
  });

  it("falls back to the breakdown sum when the capsule total is missing", () => {
    const rows = buildErrorStatusBreakdown({ 502: 1, 429: 3 });
    assert.equal(rows[0].percent, 75);
    assert.equal(rows[1].percent, 25);
  });
});

describe("formatErrorStatusPercent", () => {
  it("keeps one decimal except at the edges", () => {
    assert.equal(formatErrorStatusPercent(80.84), "80.8%");
    assert.equal(formatErrorStatusPercent(100), "100%");
    assert.equal(formatErrorStatusPercent(0), "0%");
  });
});

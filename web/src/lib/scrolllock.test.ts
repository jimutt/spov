import { describe, expect, test } from "vitest";
import { lockScroll } from "./scrolllock";

describe("the page's scroll lock", () => {
  // @decision(DL-167)
  test("is released only when every holder has let go, in either order", () => {
    document.body.style.overflow = "";
    const a = lockScroll();
    const b = lockScroll();
    expect(document.body.style.overflow).toBe("hidden");
    a();
    expect(document.body.style.overflow).toBe("hidden");
    b();
    expect(document.body.style.overflow).toBe("");
    const c = lockScroll();
    const d = lockScroll();
    d();
    c();
    c();
    expect(document.body.style.overflow).toBe("");
  });
});

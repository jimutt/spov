// Scenario 3, step 10 (DL-022): make the browser's session create answer
// ambiguously. The request still reaches the server and the session is
// started; the answer comes back without the session's id, as a proxy
// that mangled or cut the response would leave it. Run it in a
// Playwright page where member is signed in, before pressing Start:
// paste it into Playwright MCP's run-code tool, or call it from a script
// with the page.
//
// @decision(DL-022) @decision(DL-061)
async (page) => {
  await page.route("**/api/v1/sessions", async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    const res = await route.fetch();
    const body = await res.json();
    delete body.id;
    await route.fulfill({ response: res, json: body });
  });
  return "the next session create answers without its id";
}

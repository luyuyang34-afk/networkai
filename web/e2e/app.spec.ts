import { expect, test, type Page } from "@playwright/test";

const unique = () => Math.random().toString(36).slice(2, 8);

async function signUp(page: Page) {
  const username = `e2e_${unique()}`;
  await page.goto("/signup");
  await page.getByLabel("Username").fill(username);
  await page.getByLabel("Password").fill("password123");
  await page.getByRole("button", { name: "Sign up" }).click();
  await expect(page.getByRole("link", { name: `@${username}` })).toBeVisible();
  return username;
}

test("sign up, post, edit, delete", async ({ page }) => {
  const username = await signUp(page);
  const text = `hello from playwright ${unique()}`;

  await page.getByLabel("New post").fill(text);
  await page.getByRole("button", { name: "Post" }).click();
  const post = page.getByTestId("post").filter({ hasText: text });
  await expect(post).toBeVisible();
  await expect(post.getByRole("link", { name: `@${username}` })).toBeVisible();

  // Edit in place.
  await post.getByRole("button", { name: "Edit post" }).click();
  await post.getByLabel("Edit post").fill(`${text} (edited)`);
  await post.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText(`${text} (edited)`)).toBeVisible();
  await expect(post.getByText("edited", { exact: false }).first()).toBeVisible();

  // The edit survives a reload (it really reached the server).
  await page.reload();
  await expect(page.getByText(`${text} (edited)`)).toBeVisible();

  // Delete (confirm dialog accepted).
  page.once("dialog", (d) => d.accept());
  await page.getByTestId("post").filter({ hasText: text }).getByRole("button", { name: "Delete post" }).click();
  await expect(page.getByText(text)).toHaveCount(0);
  await page.reload();
  await expect(page.getByText(text)).toHaveCount(0);
});

test("a new post becomes searchable", async ({ page }) => {
  await signUp(page);
  const word = `zebra${unique()}`;
  await page.getByLabel("New post").fill(`spotted a ${word} today`);
  await page.getByRole("button", { name: "Post" }).click();
  await expect(page.getByText(`spotted a ${word} today`)).toBeVisible();

  // Search is eventually consistent (outbox → indexer → ES refresh), so
  // retry the query until the post shows up.
  await expect(async () => {
    await page.goto(`/search?q=${word}`);
    await expect(page.getByTestId("post").filter({ hasText: word })).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 15_000 });
});

test("other users' posts have no edit/delete controls", async ({ page }) => {
  await signUp(page);
  await page.goto("/u/alice"); // created by scripts/seed.sh
  await expect(page.getByTestId("post").first()).toBeVisible();
  await expect(page.getByRole("button", { name: "Edit post" })).toHaveCount(0);
});

test("errors from the API are shown, not swallowed", async ({ page }) => {
  await page.goto("/signup");
  await page.getByLabel("Username").fill("alice"); // created by scripts/seed.sh
  await page.getByLabel("Password").fill("password123");
  await page.getByRole("button", { name: "Sign up" }).click();
  // Scope to the form: Next.js also renders a role="alert" route announcer.
  await expect(page.locator("form").getByRole("alert")).toHaveText("username already exists");
});

test("expired session sends you to login", async ({ page }) => {
  await page.goto("/");
  // A syntactically valid but expired, unsigned token.
  const payload = Buffer.from(JSON.stringify({ username: "ghost", exp: 1 })).toString("base64url");
  await page.evaluate((t) => localStorage.setItem("networkai.token", t), `x.${payload}.y`);
  await page.reload();
  await expect(page.getByRole("navigation").getByRole("link", { name: "Log in" })).toBeVisible();
  await expect(page.getByLabel("New post")).toHaveCount(0);
});

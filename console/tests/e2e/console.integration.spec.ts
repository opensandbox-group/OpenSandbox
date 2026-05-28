import { expect, test } from "@playwright/test";
import { copyFileSync, mkdirSync } from "node:fs";

const shotsDir = "output/playwright";
const docsShotsDir = "../docs/public/images/console";

test.beforeAll(() => {
  mkdirSync(shotsDir, { recursive: true });
  mkdirSync(docsShotsDir, { recursive: true });
});

test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });

  await page.route("**/v1/sandboxes?**", async (route) => {
    const url = route.request().url();
    if (url.includes("state=Running")) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          items: [
            {
              id: "sbx-running",
              image: { uri: "python:3.11" },
              status: { state: "Running" },
              metadata: { "access.owner": "alice", project: "docs-demo" },
              entrypoint: ["python", "-V"],
              expiresAt: "2030-01-01T00:00:00Z",
              createdAt: "2029-12-31T00:00:00Z",
            },
          ],
          pagination: { page: 1, pageSize: 20, totalItems: 1, totalPages: 1, hasNextPage: false },
        }),
      });
      return;
    }

    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          {
            id: "sbx-001",
            image: { uri: "python:3.11" },
            status: { state: "Running" },
            metadata: { "access.owner": "alice", project: "docs-demo" },
            entrypoint: ["python", "-V"],
            expiresAt: "2030-01-01T00:00:00Z",
            createdAt: "2029-12-31T00:00:00Z",
          },
          {
            id: "sbx-002",
            image: { uri: "node:20" },
            status: { state: "Failed" },
            metadata: { "access.owner": "alice", project: "docs-demo" },
            entrypoint: ["node", "-v"],
            expiresAt: "2030-01-01T00:00:00Z",
            createdAt: "2029-12-31T00:00:00Z",
          },
        ],
        pagination: { page: 1, pageSize: 20, totalItems: 2, totalPages: 1, hasNextPage: false },
      }),
    });
  });

  await page.route("**/v1/sandboxes/sbx-001", async (route) => {
    if (route.request().method() === "DELETE") {
      await route.fulfill({ status: 204, body: "" });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        id: "sbx-001",
        image: { uri: "python:3.11" },
        status: { state: "Running" },
        metadata: { "access.owner": "alice", project: "docs-demo" },
        entrypoint: ["python", "-V"],
        expiresAt: "2030-01-01T00:00:00Z",
        createdAt: "2029-12-31T00:00:00Z",
      }),
    });
  });

  await page.route("**/v1/sandboxes/sbx-001/endpoints/**", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ endpoint: "sandbox.example.com/sbx-001/8080" }),
    });
  });

  await page.route("**/v1/sandboxes/sbx-001/renew-expiration", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ expiresAt: "2030-01-02T00:00:00Z" }),
    });
  });

  await page.route("**/v1/sandboxes/sbx-001/pause", async (route) => {
    await route.fulfill({ status: 202, body: "" });
  });
  await page.route("**/v1/sandboxes/sbx-001/resume", async (route) => {
    await route.fulfill({ status: 202, body: "" });
  });

  await page.route("**/v1/sandboxes", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify({
          id: "sbx-created",
          status: { state: "Pending" },
          metadata: { "access.owner": "alice" },
          expiresAt: "2030-01-01T00:00:00Z",
          createdAt: "2029-12-31T00:00:00Z",
          entrypoint: ["python", "-V"],
        }),
      });
      return;
    }
    await route.fallback();
  });

  await page.route("**/v1/sandboxes/sbx-created", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        id: "sbx-created",
        image: { uri: "python:3.11" },
        status: { state: "Pending" },
        metadata: { "access.owner": "alice" },
        entrypoint: ["python", "-V"],
        expiresAt: "2030-01-01T00:00:00Z",
        createdAt: "2029-12-31T00:00:00Z",
      }),
    });
  });
});

function saveScreenshot(tempPath: string, fileName: string) {
  const finalPath = `${shotsDir}/${fileName}`;
  const docsPath = `${docsShotsDir}/${fileName}`;
  copyFileSync(tempPath, finalPath);
  copyFileSync(tempPath, docsPath);
}

test("list/detail/create lifecycle flows render and work", async ({ page }, testInfo) => {
  await page.goto("/console/");
  await expect(page.getByRole("heading", { name: "OpenSandbox Console" })).toBeVisible();
  await expect(page.getByRole("link", { name: "sbx-001" })).toBeVisible();
  const listShot = testInfo.outputPath("console-list.png");
  await page.screenshot({ path: listShot, fullPage: true });
  saveScreenshot(listShot, "console-list.png");

  await page.getByRole("link", { name: "sbx-001" }).click();
  await expect(page.getByRole("heading", { name: "sbx-001" })).toBeVisible();
  await page.fill('input[placeholder="2030-01-01T12:00:00Z"]', "2030-01-02T00:00:00Z");
  await page.getByRole("button", { name: "Renew" }).click();
  await page.fill("input", "8080");
  await page.getByRole("button", { name: "Get endpoint" }).click();
  await expect(page.getByText("sandbox.example.com/sbx-001/8080")).toBeVisible();
  const detailShot = testInfo.outputPath("console-detail.png");
  await page.screenshot({ path: detailShot, fullPage: true });
  saveScreenshot(detailShot, "console-detail.png");

  await page.getByRole("link", { name: "Create" }).click();
  await page.getByRole("button", { name: "Create" }).click();
  await expect(page.getByRole("heading", { name: "sbx-created" })).toBeVisible();
  const createShot = testInfo.outputPath("console-create.png");
  await page.screenshot({ path: createShot, fullPage: true });
  saveScreenshot(createShot, "console-create.png");
});

test("auth misconfiguration banner is shown on 401 missing trusted identity", async ({ page }, testInfo) => {
  await page.unroute("**/v1/sandboxes?**");
  await page.route("**/v1/sandboxes?**", async (route) => {
    await route.fulfill({
      status: 401,
      contentType: "application/json",
      body: JSON.stringify({ code: "MISSING_TRUSTED_IDENTITY", message: "missing trusted headers" }),
    });
  });

  await page.goto("/console/");
  await expect(page.getByText("Authentication required.")).toBeVisible();
  const authShot = testInfo.outputPath("console-auth-error.png");
  await page.screenshot({ path: authShot, fullPage: true });
  saveScreenshot(authShot, "console-auth-error.png");
});

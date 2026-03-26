#!/usr/bin/env node
// Headless browser test: navigates to a magic-link URL and verifies the user
// lands on the dashboard (not the login page).
//
// Usage: node verify_dashboard.mjs <magic-link-url>
//
// Exit 0 = dashboard loaded (user is authenticated).
// Exit 1 = landed on login/magic-link page or timed out.

import { chromium } from 'playwright';

const url = process.argv[2];
if (!url) {
  console.error('Usage: node verify_dashboard.mjs <url>');
  process.exit(1);
}

const browser = await chromium.launch({
  args: ['--no-sandbox', '--disable-setuid-sandbox'],
});
const page = await browser.newPage();

try {
  // Navigate to the magic-link URL. The frontend exchanges the token,
  // stores the JWT, and does a hard redirect (window.location.href) to
  // /dashboard. We use waitUntil: 'load' since the redirect will trigger
  // a new navigation.
  await page.goto(url, { waitUntil: 'load', timeout: 15000 });

  // Wait for the URL to settle on /dashboard. The magic-link page does a
  // hard redirect after exchanging the token, so we may need to wait for
  // the second navigation.
  try {
    await page.waitForURL('**/dashboard**', { timeout: 15000 });
  } catch {
    // If we're already on /dashboard, waitForURL may have raced — check below.
  }

  const finalURL = page.url();

  if (finalURL.includes('/dashboard')) {
    console.log(`OK: landed on dashboard (${finalURL})`);
    await browser.close();
    process.exit(0);
  }

  // If we ended up on login or magic-link, auth failed.
  const bodyText = await page.textContent('body').catch(() => '');
  console.error(`FAIL: expected /dashboard, got ${finalURL}`);
  console.error(`Page text: ${(bodyText || '').slice(0, 300)}`);
  await browser.close();
  process.exit(1);
} catch (err) {
  console.error(`FAIL: ${err.message}`);
  await browser.close();
  process.exit(1);
}

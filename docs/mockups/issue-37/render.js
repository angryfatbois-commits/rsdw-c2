const path = require('path');
const fs = require('fs');
const { chromium } = require('playwright');

const root = __dirname;
const outputDir = path.join(root, 'png');
const views = [
  ['01-backups-overview', 'overview'],
  ['02-backup-profiles', 'profiles'],
  ['03-backup-profile-form', 'profile-form'],
  ['04-backup-schedules', 'schedules'],
  ['05-backup-schedule-form', 'schedule-form'],
  ['06-run-backup-form', 'run-form'],
  ['07-backup-settings', 'settings'],
  ['08-scheduled-run-now', 'scheduled-run-form'],
  ['09-backup-details', 'backup-details'],
  ['10-schedule-delete-confirmation', 'schedule-delete'],
  ['11-maintenance-restore-from-backup', 'maintenance-restore-backup'],
  ['12-maintenance-restore-custom-sav', 'maintenance-restore-custom'],
  ['13-create-server-from-backup', 'new-server-from-backup'],
];

fs.mkdirSync(outputDir, { recursive: true });

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1600, height: 1100 }, deviceScaleFactor: 1 });
  for (const [name, view] of views) {
    await page.goto(`file://${path.join(root, 'mockups.html')}?view=${view}`);
    await page.screenshot({ path: path.join(outputDir, `${name}.png`), fullPage: true });
  }
  await browser.close();
  console.log(`Rendered ${views.length} mockups to ${outputDir}`);
})();

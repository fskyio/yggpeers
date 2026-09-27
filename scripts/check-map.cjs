// Browser integration check against a running app; see scripts/README.md.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium } = require('playwright');

const baseURL = process.env.MAP_URL || 'http://127.0.0.1:8080';
const output = process.env.MAP_QA_DIR || '/tmp/yggpeers-map-qa';
const fixtures = {
	'Hong Kong': { total: 12, online: 4, avgUptime: 80 },
	Singapore: { total: 8, online: 3, avgUptime: 95 },
	Monaco: { total: 3, online: 0, avgUptime: null },
	Germany: { total: 24, online: 18, avgUptime: 96 },
	'United States': { total: 30, online: 24, avgUptime: 92 },
	Russia: { total: 22, online: 10, avgUptime: 78 },
	Sweden: { total: 4, online: 2, avgUptime: 90 },
	Australia: { total: 9, online: 6, avgUptime: 88 },
	Brazil: { total: 3, online: 1, avgUptime: 75 }
};

async function run() {
	await fs.mkdir(output, { recursive: true });
	const browser = await chromium.launch({
		headless: true,
		executablePath: process.env.CHROMIUM_PATH || undefined,
		args: ['--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader']
	});
	try {
		const page = await browser.newPage({ viewport: { width: 1600, height: 900 }, colorScheme: 'light' });
		const errors = [];
		const ranges = [];
		page.on('pageerror', error => errors.push(error.message));
		page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
		page.on('response', response => {
			if (response.url().endsWith('/static/countries.pmtiles')) {
				ranges.push({ status: response.status(), bytes: Number(response.headers()['content-length']), range: response.headers()['content-range'] });
			}
		});
		await page.route('**/api/countries', route => route.fulfill({ json: fixtures }));
		// Expose the real map only in this test, without adding a production test hook.
		await page.route('**/maplibre-gl.js', async route => {
			const response = await route.fetch();
			const body = await response.text() + '\n{ const OriginalMap = maplibregl.Map; maplibregl.Map = class extends OriginalMap { constructor(options) { super(options); window.__peerMap = this; } }; }';
			await route.fulfill({ response, body });
		});
		const catalog = await (await fetch(baseURL + '/static/countries.json')).json();
		async function idle() {
			await page.waitForFunction(() => window.__peerMap && window.__peerMap.loaded() && !window.__peerMap.isMoving());
		}
		async function point(code) {
			return page.evaluate(coordinates => {
				const p = window.__peerMap.project(coordinates);
				const rect = window.__peerMap.getCanvas().getBoundingClientRect();
				return { x: p.x + rect.left, y: p.y + rect.top };
			}, catalog[code].marker);
		}
		async function features(layer) {
			return page.evaluate(layer => window.__peerMap.queryRenderedFeatures({ layers: [layer] }).map(f => f.properties.code), layer);
		}
		async function snapshot(name) {
			await page.screenshot({ path: path.join(output, name + '.png') });
		}
		const samples = [];
		let offset = 0;
		function sample(view) {
			const requests = ranges.slice(offset);
			samples.push({ view, requests: requests.length, archiveBytes: requests.reduce((n, r) => n + r.bytes, 0) });
			offset = ranges.length;
		}

		await page.goto(baseURL + '/map');
		await page.locator('[data-mode="online"]:enabled').waitFor();
		await idle();
		assert.equal(await page.locator('#map-status').isVisible(), false);
		for (const code of ['HK', 'SG', 'MC']) assert.ok((await features('small-places')).includes(code), code + ' marker missing');
		await snapshot('world');
		sample('World, zoom 2, cold cache');

		const hk = await point('HK');
		await page.mouse.move(hk.x, hk.y);
		await page.locator('.maplibregl-popup-content').filter({ hasText: 'Hong Kong' }).waitFor();
		assert.match(await page.locator('.maplibregl-popup-content').textContent(), /12 total peers/);
		await page.mouse.click(hk.x, hk.y);
		await page.waitForFunction(() => window.__peerMap.getZoom() >= 8);
		await idle();
		assert.ok((await features('countries-fill')).includes('HK'), 'Hong Kong polygon missing');
		assert.ok(!(await features('small-places')).includes('HK'), 'Hong Kong marker should give way to its polygon');
		await snapshot('hong-kong');
		sample('Hong Kong, zoom 8, after world view');

		const beforeToggle = ranges.length;
		await page.getByRole('button', { name: 'Online', exact: true }).click();
		await idle();
		assert.equal(ranges.length, beforeToggle, 'changing peer mode must not download geometry again');
		await page.evaluate(coordinates => window.__peerMap.jumpTo({ center: coordinates, zoom: 8 }), catalog.SG.marker);
		await idle();
		assert.ok((await features('countries-fill')).includes('SG'), 'Singapore polygon missing');
		const sg = await point('SG');
		await page.mouse.move(sg.x, sg.y);
		await page.locator('.maplibregl-popup-content').filter({ hasText: 'Singapore' }).waitFor();
		assert.match(await page.locator('.maplibregl-popup-content').textContent(), /8 total peers.*3 online/);
		const state = await page.evaluate(() => window.__peerMap.getFeatureState({ source: 'countries', sourceLayer: 'countries', id: 'SG' }));
		assert.equal(state.online, 3, 'peer state must apply to tiles loaded after toggling');
		await snapshot('singapore');
		sample('Singapore, zoom 8, after Hong Kong');

		await page.evaluate(() => window.__peerMap.jumpTo({ center: [0, 30], zoom: 2 }));
		await idle();
		assert.ok(!(await features('small-places')).includes('MC'), 'offline-only place should not have an Online marker');
		await page.getByRole('button', { name: 'Total', exact: true }).click();
		await idle();
		assert.ok((await features('small-places')).includes('MC'), 'Total should restore offline-only markers');
		assert.equal(await page.locator('[data-mode="total"]').getAttribute('aria-pressed'), 'true');
		assert.ok(ranges.length > 0);
		assert.ok(ranges.every(r => r.status === 206 && r.bytes > 0 && r.bytes < 500000), 'archive must load in small partial responses');
		assert.ok(samples[0].archiveBytes < 300000, 'world-view archive transfer exceeded 300 KB');
		assert.deepEqual(errors, []);

		// A failing counts API must leave the basemap visible and explain the problem.
		await page.route('**/api/countries', route => route.fulfill({ status: 503, body: 'unavailable' }));
		await page.reload();
		await page.locator('#map-status').filter({ hasText: 'Peer counts could not be loaded' }).waitFor();
		await idle();
		assert.ok((await features('countries-base')).length > 0);
		assert.equal(await page.locator('[data-mode="online"]').isDisabled(), true);

		await page.route('**/api/countries', route => route.fulfill({ json: {} }));
		await page.reload();
		await page.locator('[data-mode="online"]:enabled').waitFor();
		await idle();
		assert.equal((await features('small-places')).length, 0, 'empty peer data should not create markers');
		assert.equal(await page.locator('#map-status').isVisible(), false);

		await page.route('**/api/countries', route => route.fulfill({ json: fixtures }));
		await page.setViewportSize({ width: 390, height: 844 });
		await page.reload();
		await page.locator('[data-mode="online"]:enabled').waitFor();
		await idle();
		await snapshot('mobile');
		assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'mobile layout must fit the viewport');

		await fs.writeFile(path.join(output, 'transfers.json'), JSON.stringify({ viewport: '1600x900', samples, ranges }, null, 2) + '\n');
		console.log(JSON.stringify(samples, null, 2));
		console.log('Map browser checks passed. Screenshots and transfer log:', output);
	} finally {
		await browser.close();
	}
}

run().catch(error => { console.error(error); process.exitCode = 1; });

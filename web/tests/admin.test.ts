import {expect, test} from '@playwright/test';
import type {Component, ComponentStatus, Environment, Resource, RuntimeInstance, Site} from '../src/api';

test('native resource editing preserves identity and write-only credentials', async ({page}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const sites: Resource<Site>[] = [];
  const environments: Resource<Environment>[] = [];
  const components: Resource<Component, ComponentStatus>[] = [
    {
      name: 'gateway',
      uid: 'gateway-uid',
      resourceVersion: '1',
      generation: 1,
      deleting: false,
      spec: {
        role: 'gateway',
        displayName: 'Primary gateway',
        gateway: {
          http: {listen: ':8080', public_url: 'https://workspace.example.com'},
          ssh: {
            listen: ':2222',
            public_addr: 'ssh.example.com:2222',
            handshake_timeout: '30s',
            max_channels_per_connection: 32,
            auth: {
              max_attempts_per_ip_per_minute: 30,
              max_attempts_per_codespace_per_minute: 20,
              max_attempts_per_ip_codespace_per_minute: 10,
              max_attempts_per_public_key_per_minute: 30,
              failure_window: '10m',
            },
          },
          sessions: {
            ttl: '8h',
            idle_timeout: '30m',
            revalidate_interval: '5m',
            max_per_codespace: 32,
            max_per_user: 128,
          },
          limits: {
            max_inflight_total: 4096,
            max_inflight_per_session: 32,
            public_max_connections_per_endpoint: 64,
            public_max_connections_per_ip: 16,
            validation_max_inflight: 128,
          },
        },
      },
      status: {available: true, readyReplicas: 1, desiredReplicas: 1},
    },
  ];
  const runtimes: Resource<RuntimeInstance>[] = [
    {
      name: 'runtime-one',
      namespace: 'codespace-production',
      uid: 'runtime-uid',
      resourceVersion: '8',
      generation: 2,
      deleting: false,
      spec: {
        site: {name: 'production', uid: 'site-uid'},
        codespaceID: '42',
        runtimeUUID: '24d0874b-ad2a-4569-82ee-204ca30fb288',
        environmentTag: 'standard',
        operation: 'stop',
        version: 3,
      },
      status: {
        ready: false,
        pod: {name: 'runtime-pod', uid: 'pod-uid'},
        conditions: [
          {
            type: 'InfrastructureReady',
            status: 'False',
            reason: 'WriterUnconfirmed',
            message: 'previous writer has not been confirmed stopped',
          },
        ],
      },
    },
  ];
  let signedIn = false;
  let lastWrite: {managerSecret?: string; uid?: string; resourceVersion?: string; spec?: Site} = {};
  let recoveryWrite: Record<string, string> = {};
  await page.route('**/api/admin/**', async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace('/api/admin/', '');
    if (path === 'login') {
      signedIn = true;
      await route.fulfill({json: {csrf: 'test-csrf'}});
      return;
    }
    if (!signedIn) {
      await route.fulfill({status: 401, json: {error: 'sign in required'}});
      return;
    }
    if (path === 'session') {
      if (request.method() === 'DELETE') {
        signedIn = false;
        await route.fulfill({status: 204});
      } else await route.fulfill({json: {csrf: 'test-csrf'}});
      return;
    }
    const [kind, name, action] = path.split('/');
    if (request.method() === 'GET') {
      await route.fulfill({
        json:
          kind === 'sites'
            ? sites
            : kind === 'environments'
              ? environments
              : kind === 'components'
                ? components
                : kind === 'runtimes'
                  ? runtimes
                  : [],
      });
      return;
    }
    const body = request.postDataJSON();
    if (kind === 'runtimes' && action) {
      recoveryWrite = body;
      await route.fulfill({status: 204});
      return;
    } else if (kind === 'environments') {
      environments.push({
        name: body.name,
        uid: 'template-uid',
        resourceVersion: '1',
        generation: 1,
        deleting: false,
        spec: body.spec,
      });
    } else if (kind === 'sites') {
      lastWrite = body;
      if (name) {
        if (body.resourceVersion !== sites[0].resourceVersion) {
          await route.fulfill({status: 409, json: {error: 'resource changed; reload before saving'}});
          return;
        }
        sites[0] = {...sites[0], resourceVersion: '2', spec: body.spec};
      } else
        sites.push({
          name: body.name,
          uid: 'site-uid',
          resourceVersion: '1',
          generation: 1,
          deleting: false,
          spec: {...body.spec, credential: {name: 'credential', uid: 'credential-uid'}},
        });
    }
    await route.fulfill({json: {name: body.name}});
  });
  await page.goto('/environments');
  await page.locator('input[type=password]').fill('test-administrator-token');
  await page.getByRole('button', {name: 'Sign in', exact: true}).click();
  await page.getByRole('button', {name: 'Add template'}).click();
  const field = (label: string) =>
    page
      .locator('.n-form-item')
      .filter({has: page.locator('.n-form-item-label', {hasText: new RegExp(`^${label}$`)})})
      .getByRole('textbox');
  await field('Resource name').fill('standard');
  await field('Tag').fill('standard');
  await field('Runtime image digest').fill(`registry.example.com/runtime@sha256:${'a'.repeat(64)}`);
  await field('StorageClass').fill('storage');
  await field('code-server version').fill('4.121.0');
  await page.getByRole('button', {name: 'Save template'}).click();
  await expect(page.getByText('Pending verification', {exact: true})).toBeVisible();
  await page.getByRole('link', {name: 'Gitea sites', exact: true}).click();
  await page.getByRole('button', {name: 'Add site'}).click();
  await field('Resource name').fill('production');
  await field('Display name').fill('Production Gitea');
  await field('Gitea URL').fill('https://gitea.example.com');
  await field('Manager ID').fill('9007199254740993');
  await page.getByPlaceholder('Leave blank to retain the configured secret').fill('private-manager-secret');
  await page
    .locator('.n-form-item')
    .filter({hasText: /^Environment templates/})
    .locator('.n-base-selection')
    .click();
  await page.getByText('standard (standard)', {exact: true}).click();
  await page.keyboard.press('Escape');
  await page
    .locator('.n-form-item')
    .filter({hasText: /^Gateway/})
    .locator('.n-base-selection')
    .click();
  await page.getByText('gateway', {exact: true}).click();
  await page.getByRole('button', {name: 'Save site'}).click();
  await expect(page.getByText('Manager 9007199254740993', {exact: true})).toBeVisible();
  expect(lastWrite.spec!.managerID).toBe('9007199254740993');
  expect(lastWrite.managerSecret).toBe('private-manager-secret');
  await page.getByRole('button', {name: 'Edit site', exact: true}).click();
  await expect(page.getByPlaceholder('Leave blank to retain the configured secret')).toHaveValue('');
  await field('Display name').fill('Renamed Gitea');
  await page.getByRole('button', {name: 'Save site'}).click();
  await expect(page.getByText('Renamed Gitea', {exact: true})).toBeVisible();
  expect(lastWrite.uid).toBe('site-uid');
  expect(lastWrite.resourceVersion).toBe('1');
  expect(lastWrite.managerSecret).toBe('');
  await page.screenshot({path: 'test-results/native-sites-desktop.png', fullPage: true});
  await page.setViewportSize({width: 390, height: 844});
  await page.getByRole('button', {name: 'Edit site', exact: true}).click();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({path: 'test-results/native-site-mobile.png', fullPage: true});
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.getByRole('link', {name: 'Components', exact: true}).click();
  await expect(page.getByRole('cell', {name: 'https://workspace.example.com'})).toBeVisible();
  await expect(page.getByText('Available', {exact: true})).toBeVisible();
  await page.getByRole('button', {name: 'Edit component', exact: true}).click();
  await expect(field('Public HTTPS URL')).toHaveValue('https://workspace.example.com');
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.getByRole('link', {name: 'Running environments', exact: true}).click();
  const runtimeRow = page.getByRole('row').filter({hasText: 'runtime-one'});
  const runtimeCells = runtimeRow.getByRole('cell');
  await expect(runtimeCells).toHaveCount(6);
  await expect(runtimeCells.nth(0)).toContainText('42');
  await expect(runtimeCells.nth(1)).toHaveText('production');
  await expect(runtimeCells.nth(2)).toHaveText('standard');
  await expect(runtimeCells.nth(3)).toHaveText('stop #3');
  await expect(runtimeCells.nth(4)).toContainText('runtime-pod');
  await expect(runtimeCells.nth(5).getByRole('button', {name: 'Confirm stopped'})).toBeVisible();
  await runtimeRow.getByRole('button', {name: 'Confirm stopped'}).click();
  await page.getByRole('button', {name: 'Confirm stopped', exact: true}).last().click();
  await expect
    .poll(() => recoveryWrite)
    .toEqual({
      uid: 'runtime-uid',
      resourceVersion: '8',
      podUID: 'pod-uid',
    });
  await page.getByRole('button', {name: 'Sign out'}).click();
  await expect(page.getByRole('button', {name: 'Sign in', exact: true})).toBeVisible();
  expect(errors).toEqual([]);
});

import {expect, test} from '@playwright/test';
import type {
  ClusterOptions,
  Component,
  ComponentStatus,
  Environment,
  Resource,
  RuntimeInstance,
  RuntimeStatus,
  Site,
} from '../src/api';

test('administration workflows preserve resource identity and validate cluster-backed selections', async ({page}) => {
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
        resources: {requests: {cpu: '50m', memory: '64Mi'}, limits: {cpu: '1', memory: '512Mi'}},
        gateway: {
          replicas: 2,
          endpointBaseURL: 'https://workspace.example.com',
          sshExternalAddress: 'ssh.example.com:22',
          httpExposure: {mode: 'external'},
          sshExposure: {mode: 'external'},
          ssh: {
            handshakeTimeout: '30s',
            maxChannelsPerConnection: 32,
            auth: {
              maxAttemptsPerIP: 30,
              maxAttemptsPerCodespace: 20,
              maxAttemptsPerIPCodespace: 10,
              maxAttemptsPerPublicKey: 30,
              failureWindow: '10m',
            },
          },
          sessions: {
            ttl: '8h',
            idleTimeout: '30m',
            revalidateInterval: '5m',
            maxPerCodespace: 32,
            maxPerUser: 128,
          },
          limits: {
            maxInflightTotal: 4096,
            maxInflightPerSession: 32,
            publicMaxConnectionsPerEndpoint: 64,
            publicMaxConnectionsPerIP: 16,
            validationMaxInflight: 128,
          },
        },
      },
      status: {available: true, readyReplicas: 2, desiredReplicas: 2},
    },
  ];
  const runtimes: Resource<RuntimeInstance, RuntimeStatus>[] = [
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
        operation: 'resume',
        version: 3,
      },
      status: {
        ready: false,
        canStop: true,
        stopping: false,
        stopped: false,
        deleting: false,
        pod: {name: 'runtime-pod', uid: 'pod-uid'},
        conditions: [],
      },
    },
  ];
  const clusterOptions: ClusterOptions = {
    runtimeClasses: [{name: 'kata-qemu', handler: 'kata-qemu', isolation: 'kata'}],
    storageClasses: [{name: 'storage', provisioner: 'test.csi', default: true}],
    deviceResources: [],
    requiredRuntimeCapabilities: ['SYS_ADMIN', 'SYS_RESOURCE'],
    ingressClasses: [{name: 'nginx', controller: 'k8s.io/ingress-nginx', default: true}],
    tlsSecrets: ['wildcard-tls'],
    issuers: [{kind: 'ClusterIssuer', name: 'letsencrypt'}],
  };
  let signedIn = false;
  let lastWrite: {registrationToken?: string; uid?: string; resourceVersion?: string; spec?: Site} = {};
  let runtimeWrite: Record<string, string> | undefined;
  let rotationWrite: Record<string, string> = {};
  let failNextEnvironmentWrite = true;
  const requestedLanguages: string[] = [];
  await page.route('**/api/admin/**', async (route) => {
    const request = route.request();
    requestedLanguages.push(request.headers()['accept-language'] ?? '');
    const path = new URL(request.url()).pathname.replace('/api/admin/', '');
    if (path === 'login') {
      signedIn = true;
      await route.fulfill({json: {csrf: 'test-csrf'}});
      return;
    }
    if (!signedIn) {
      await route.fulfill({status: 401, json: {code: 'authentication_required', message: 'sign in required'}});
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
          kind === 'cluster-options'
            ? clusterOptions
            : kind === 'sites'
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
      runtimeWrite = body;
      if (request.method() === 'POST') {
        runtimes[0].status = {...runtimes[0].status, canStop: false, stopping: true};
      }
      await route.fulfill({status: 204});
      return;
    } else if (kind === 'components' && action === 'rotate-ssh-host-key') {
      rotationWrite = body;
    } else if (kind === 'environments') {
      if (failNextEnvironmentWrite) {
        failNextEnvironmentWrite = false;
        await route.fulfill({status: 503, json: {code: 'request_failed', message: 'temporary failure'}});
        return;
      }
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
          await route.fulfill({
            status: 409,
            json: {code: 'resource_conflict', message: 'resource changed; reload before saving'},
          });
          return;
        }
        sites[0] = {...sites[0], resourceVersion: '2', spec: body.spec};
      } else
        sites.push({
          name: 'site-generated',
          uid: 'site-uid',
          resourceVersion: '1',
          generation: 1,
          deleting: false,
          spec: body.spec,
        });
    }
    await route.fulfill({
      json:
        kind === 'environments'
          ? environments.at(-1)
          : kind === 'sites'
            ? name
              ? sites[0]
              : sites.at(-1)
            : {name: body.name},
    });
  });
  await page.goto('/environments');
  await expect(page.locator('.login .language-select')).toBeVisible();
  await page.locator('input[type=password]').fill('test-administrator-token');
  await page.getByRole('button', {name: 'Sign in', exact: true}).click();
  await page.getByRole('button', {name: 'Add template'}).click();
  const field = (label: string) =>
    page
      .locator('.n-form-item')
      .filter({has: page.locator('.n-form-item-label', {hasText: new RegExp(`^${label}$`)})})
      .getByRole('textbox');
  const selectField = (label: string) =>
    page
      .locator('.n-form-item')
      .filter({has: page.locator('.n-form-item-label', {hasText: new RegExp(`^${label}$`)})})
      .locator('.n-base-selection');
  await expect(selectField('Isolation')).toBeVisible();
  await expect(selectField('RuntimeClass')).toBeVisible();
  await expect(selectField('StorageClass')).toBeVisible();
  await selectField('Isolation').click();
  await page.getByText('Native', {exact: true}).last().click();
  await expect(selectField('RuntimeClass')).not.toBeVisible();
  const runtimeCapabilities = selectField('Runtime Pod capabilities');
  await expect(runtimeCapabilities.getByText('SYS_ADMIN (required)', {exact: true})).toBeVisible();
  await runtimeCapabilities.click();
  await page.keyboard.type('SYS_MODULE');
  await page.keyboard.press('Enter');
  await page.keyboard.press('Escape');
  const devContainerCapabilities = selectField('Dev Container capabilities');
  await devContainerCapabilities.click();
  await page.keyboard.type('SYS_PTRACE');
  await page.keyboard.press('Enter');
  await page.keyboard.press('Escape');
  await field('Tag').fill('standard');
  await page.getByRole('textbox', {name: 'Initial Web IDE settings JSON'}).fill('{"editor.tabSize":2}');
  await field('Default extensions').fill('golang.go\nredhat.vscode-yaml');
  await page.getByRole('checkbox', {name: 'Install the Gitea CLI'}).check();
  await page.getByRole('checkbox', {name: 'Use an outbound proxy'}).check();
  await field('HTTP proxy').fill('http://proxy-user:proxy-password@proxy.example:3128');
  await page.getByRole('button', {name: 'Save template'}).click();
  await expect(page.getByText('Request failed', {exact: false})).toBeVisible();
  await expect(field('Tag')).toHaveValue('standard');
  await expect(field('HTTP proxy')).toHaveValue('http://proxy-user:proxy-password@proxy.example:3128');
  await field('Direct destinations').fill('registry.internal.example');
  await page.getByRole('button', {name: 'Save template'}).click();
  await expect(page.getByText('Waiting for status', {exact: true})).toBeVisible();
  expect(environments[0].spec.runtime.devContainer.webIDE).toEqual({
    enabled: true,
    settings: {'editor.tabSize': 2},
    extensions: ['golang.go', 'redhat.vscode-yaml'],
  });
  expect(environments[0].spec.runtime.devContainer.giteaCLI).toBe(true);
  expect(environments[0].spec.runtime.resources).toEqual({
    requests: {cpu: '100m', memory: '256Mi'},
  });
  expect(environments[0].spec.runtime.native).toMatchObject({
    runtimeCapAdd: ['SYS_MODULE'],
    devContainerCapAdd: ['SYS_PTRACE'],
  });
  expect(environments[0].spec.runtime.proxy).toEqual({
    httpProxy: 'http://proxy-user:proxy-password@proxy.example:3128',
    noProxy: ['registry.internal.example'],
  });
  await page.locator('.language-select .n-base-selection').click();
  await page.getByText('简体中文', {exact: true}).last().click();
  await expect(page.getByRole('link', {name: 'Gitea 站点', exact: true})).toBeVisible();
  await page.getByRole('button', {name: '刷新模板'}).click();
  await expect.poll(() => requestedLanguages.at(-1)).toBe('zh-CN');
  await page.locator('.language-select .n-base-selection').click();
  await page.getByText('English', {exact: true}).last().click();
  await page.getByRole('link', {name: 'Gitea sites', exact: true}).click();
  await page.getByRole('button', {name: 'Add site'}).click();
  await field('Display name').fill('Production Gitea');
  await field('Gitea URL').fill('https://gitea.example.com');
  await field('Registration token').fill('0000000000000000000000000000000000000000');
  await page
    .locator('.n-form-item')
    .filter({hasText: /^Environment templates/})
    .locator('.n-base-selection')
    .click();
  await page.getByText('standard', {exact: true}).last().click();
  await page.keyboard.press('Escape');
  await page
    .locator('.n-form-item')
    .filter({hasText: /^Gateway/})
    .locator('.n-base-selection')
    .click();
  await page.getByText('Primary gateway', {exact: true}).click();
  await page.getByRole('button', {name: 'Save site'}).click();
  await expect(page.getByText('Production Gitea', {exact: true})).toBeVisible();
  expect(lastWrite.registrationToken).toBe('0000000000000000000000000000000000000000');
  await page.getByRole('button', {name: 'Edit site', exact: true}).click();
  await expect(field('Registration token')).toHaveCount(0);
  await field('Display name').fill('Renamed Gitea');
  await page.getByRole('button', {name: 'Save site'}).click();
  await expect(page.getByText('Renamed Gitea', {exact: true})).toBeVisible();
  expect(lastWrite.uid).toBe('site-uid');
  expect(lastWrite.resourceVersion).toBe('1');
  expect(lastWrite.registrationToken).toBe('');
  await page.setViewportSize({width: 390, height: 844});
  await page.getByRole('button', {name: 'Edit site', exact: true}).click();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.getByRole('link', {name: 'Components', exact: true}).click();
  await expect(page.getByRole('cell', {name: 'https://workspace.example.com'})).toBeVisible();
  await expect(page.getByText('Available', {exact: true})).toBeVisible();
  await expect(page.getByText('2 of 2 replicas ready', {exact: true})).toBeVisible();
  await page.getByRole('button', {name: 'Rotate SSH host key', exact: true}).click();
  await page.getByRole('button', {name: 'Rotate key', exact: true}).click();
  await expect.poll(() => rotationWrite).toEqual({name: 'gateway', uid: 'gateway-uid', resourceVersion: '1'});
  await page.getByRole('button', {name: 'Edit component', exact: true}).click();
  await expect(field('Endpoint base URL')).toHaveValue('https://workspace.example.com');
  await expect(
    page
      .locator('.n-form-item')
      .filter({hasText: /^Gateway replicas/})
      .locator('input'),
  ).toHaveValue('2');
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.getByRole('button', {name: 'Add component', exact: true}).click();
  await field('Endpoint base URL').fill('https://workspace.example.com:443');
  await field('External SSH address').fill('ssh.example.com:2222');
  await expect(selectField('HTTP publication')).toBeVisible();
  await selectField('HTTP publication').click();
  await page.getByText('Create a Kubernetes Ingress', {exact: true}).click();
  await expect(field('Endpoint base URL')).toHaveValue('https://workspace.example.com');
  await expect(selectField('Ingress class')).toBeVisible();
  await expect(selectField('Ingress TLS')).toBeVisible();
  await selectField('Ingress TLS').click();
  await page.getByText('Plain HTTP', {exact: true}).click();
  await expect(field('Endpoint base URL')).toHaveValue('http://workspace.example.com');
  await expect(field('Endpoint base URL')).toHaveAttribute('placeholder', 'http://codespace.example.com');
  await selectField('Ingress TLS').click();
  await page.getByText('cert-manager issuer', {exact: true}).click();
  await expect(field('Endpoint base URL')).toHaveValue('https://workspace.example.com');
  await expect(field('Endpoint base URL')).toHaveAttribute('placeholder', 'https://codespace.example.com');
  await expect(selectField('Issuer')).toBeVisible();
  await selectField('SSH publication').click();
  await page.getByText('Create a LoadBalancer Service', {exact: true}).click();
  await expect(field('External SSH address')).toHaveValue('ssh.example.com:22');
  await selectField('Role').click();
  await page.getByText('Registry cache', {exact: true}).last().click();
  await expect(selectField('Storage class')).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.getByRole('link', {name: 'Running environments', exact: true}).click();
  const runtimeRow = page.getByRole('row').filter({hasText: 'runtime-one'});
  const runtimeCells = runtimeRow.getByRole('cell');
  await expect(runtimeCells).toHaveCount(6);
  await expect(runtimeCells.nth(0)).toContainText('42');
  await expect(runtimeCells.nth(1)).toHaveText('production');
  await expect(runtimeCells.nth(2)).toHaveText('standard');
  await expect(runtimeCells.nth(3)).toHaveText('resume #3');
  await expect(runtimeCells.nth(4)).toContainText('Starting…');
  await expect(runtimeCells.nth(4)).toContainText('runtime-pod');
  await expect(runtimeCells.nth(5).getByRole('button', {name: 'Stop'})).toBeVisible();
  await runtimeRow.getByRole('button', {name: 'Stop'}).click();
  await expect.poll(() => runtimeWrite).toEqual({uid: 'runtime-uid', podUID: 'pod-uid'});
  await expect(runtimeRow.getByRole('button', {name: 'Stopping…'})).toBeDisabled();
  runtimeWrite = undefined;
  await runtimeRow.getByRole('button', {name: 'Delete data'}).click();
  await expect(page.getByText('Disable the owning site first.', {exact: false})).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.getByRole('button', {name: 'Delete', exact: true}).click();
  await expect.poll(() => runtimeWrite).toEqual({uid: 'runtime-uid', resourceVersion: '8', podUID: 'pod-uid'});
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.setViewportSize({width: 1280, height: 900});
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.getByRole('button', {name: 'Sign out'}).click();
  await expect(page.getByRole('button', {name: 'Sign in', exact: true})).toBeVisible();
  expect(errors).toEqual([]);
});

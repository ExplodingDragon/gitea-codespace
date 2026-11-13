import { afterEach, expect, it, vi } from 'vitest'
import { CodespaceEndpointService } from '../src/index.ts'

afterEach(() => { vi.unstubAllEnvs() })

it('exposes only complete public SSH connection information', () => {
  vi.stubEnv('CODESPACE_SSH_USER', 'cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986')
  vi.stubEnv('CODESPACE_SSH_HOST', 'ssh.example.com')
  vi.stubEnv('CODESPACE_SSH_PORT', '2222')
  vi.stubEnv('GITEA_TOKEN', 'private')
  expect(CodespaceEndpointService.prototype.sshConnection()).toEqual({
    user: 'cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986', host: 'ssh.example.com', port: 2222,
  })
  vi.stubEnv('CODESPACE_SSH_HOST', '-oProxyCommand=bad')
  expect(CodespaceEndpointService.prototype.sshConnection()).toBeNull()
  vi.stubEnv('CODESPACE_SSH_HOST', '')
  expect(CodespaceEndpointService.prototype.sshConnection()).toBeNull()
})

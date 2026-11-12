import { execFile } from 'node:child_process'
import { constants, accessSync } from 'node:fs'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)

export type CommandRunner = (
  args: readonly string[], signal?: AbortSignal,
) => Promise<{ stdout: string; stderr: string }>

/** Each provider owns its subprocess boundary; browser code only uses Remote. */
export function runtimeCommand(file: string, timeout: number): CommandRunner {
  return (args, signal) => execFileAsync(file, args, {
    encoding: 'utf8', maxBuffer: 1024 * 1024, signal, timeout,
  })
}

export function commandAvailable(file: string): boolean {
  try {
    accessSync(file, constants.X_OK)
    return true
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code
    if (code === 'ENOENT' || code === 'EACCES') return false
    throw error
  }
}

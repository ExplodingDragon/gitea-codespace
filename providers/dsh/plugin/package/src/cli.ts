import { execFile } from 'node:child_process'
import { constants, accessSync } from 'node:fs'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)

export type CommandRunner = (
  args: readonly string[], signal?: AbortSignal,
) => Promise<{ stdout: string; stderr: string }>

/** Each provider owns its subprocess boundary; browser code only uses Remote. */
export function runtimeCommand(file: string, timeout: number): CommandRunner {
  return async (args, signal) => {
    try {
      return await execFileAsync(file, args, {
        encoding: 'utf8', maxBuffer: 1024 * 1024, signal, timeout,
      })
    } catch (error) {
      console.error('Codespace command failed', error)
      const stderr = (error as { stderr?: unknown } | null)?.stderr
      const detail = typeof stderr === 'string' && stderr.trim() ? stderr.trim() : error instanceof Error ? error.message : String(error)
      throw new Error(`${args.join(' ')}: ${detail}`, { cause: error })
    }
  }
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

import { defineTool, type ToolDefinition } from '@deepseek-ai/dsh-tools'
import { formatEndpoints } from './commands.ts'
import type { EndpointClient } from './endpoint.ts'

const endpointSchema = {
  type: 'object',
  additionalProperties: false,
  properties: {
    port: { type: 'integer', required: true },
    label: { type: 'string', required: true },
    visibility: { type: 'string', required: true, enum: ['authenticated', 'public'] },
  },
} as const

/** Create the model tools over one endpoint client. */
export function endpointTools(endpointClient: EndpointClient): ToolDefinition[] {
  return [
    defineTool({
      name: 'codespace_list_ports',
      description: 'List ports currently published from this Codespace and their access visibility.',
      parameters: {},
      output: {
        schema: { type: 'array', items: endpointSchema },
        render: (_args, endpoints) => [{ type: 'text', text: formatEndpoints(endpoints) }],
      },
      isConcurrencySafe: () => true,
      execute: (_args, exec) => endpointClient.list(exec.signal),
    }),
    defineTool({
      name: 'codespace_publish_port',
      description: 'Publish or update a port from this Codespace. Authenticated visibility is used unless public access is explicitly requested.',
      parameters: {
        port: { type: 'integer', required: true, description: 'Container port from 1 through 65535.' },
        label: { type: 'string', required: true, description: 'Short service label.' },
        visibility: {
          type: 'string',
          enum: ['authenticated', 'public'],
          description: 'Use authenticated by default. Public exposes the service without a Codespace user session.',
        },
      },
      output: {
        schema: endpointSchema,
        render: (_args, endpoint) => [{
          type: 'text',
          text: `Published ${endpoint.port} (${endpoint.visibility}): ${endpoint.label}`,
        }],
      },
      execute: (args, exec) => endpointClient.publish({
        port: args.port,
        label: args.label,
        visibility: args.visibility ?? 'authenticated',
      }, exec.signal),
    }),
    defineTool({
      name: 'codespace_unpublish_port',
      description: 'Remove a published Codespace port without stopping the application that listens on it.',
      parameters: {
        port: { type: 'integer', required: true, description: 'Published container port to remove.' },
      },
      output: {
        schema: {
          type: 'object',
          additionalProperties: false,
          properties: { port: { type: 'integer', required: true } },
        },
        render: (_args, result) => [{ type: 'text', text: `Unpublished port ${result.port}.` }],
      },
      execute: async (args, exec) => ({ port: await endpointClient.unpublish(args.port, exec.signal) }),
    }),
  ]
}
